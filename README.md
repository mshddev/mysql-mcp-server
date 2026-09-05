# mysql-mcp-server

> Give AI agents access to MySQL/MariaDB over MCP. One server beside the
> database, every agent on the team connects to it. Support PII masking. Read-only by default, full
> access as an explicit opt-in.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](go.mod)

An MCP server that gives AI agents access to MySQL/MariaDB database.
It is built to run as a shared service: you deploy one instance on a host near
the database, and every agent on the team points at its URL (with a bearer
token). The transport is streamable HTTP (MCP spec 2026-07-28, stateless). For
a single person and a database on the same machine it can also run as a
subprocess over stdio, with `--stdio`; see [Stdio](#stdio).

It exposes a single tool, `query`, runs the SQL you send, and returns rows as
JSON objects.

The point is to let a coding agent (Claude Code, or anything that speaks MCP)
answer real data questions and explore a schema — and with masking enabled, prevent PII data enters agent context window.

## Quickstart

Zero to a working server against a database you already have, all on one
machine, so you can watch it answer before putting it on a host. You need a
reachable MySQL or MariaDB and enough access on it to create a user. The team
setup is under [Deploy](#deploy); the steps are the same, spread across two
machines.

**1. Create a read-only user.** As an admin, with your own database name and
password:

```sql
-- Both host variants: a default MariaDB install keeps anonymous ''@'localhost'
-- users that shadow '%' users on local connections.
CREATE USER 'mcp_readonly'@'%' IDENTIFIED BY 'a-strong-password';
CREATE USER 'mcp_readonly'@'localhost' IDENTIFIED BY 'a-strong-password';
GRANT SELECT ON yourdb.* TO 'mcp_readonly'@'%';
GRANT SELECT ON yourdb.* TO 'mcp_readonly'@'localhost';
FLUSH PRIVILEGES;
```

verify:

```bash
mysql -h 127.0.0.1 -u mcp_readonly -p -e "SELECT 1"
```

**2. Install it.**

```bash
curl -fsSL https://raw.githubusercontent.com/mshddev/mysql-mcp-server/main/install.sh | sh
```

Other routes (Go install, build from source, manual download) are under
[Install](#install). Verify:

```bash
mysql-mcp-server --version
```

**3. Write the config.**

```bash
curl -fsSL -o config.yaml https://raw.githubusercontent.com/mshddev/mysql-mcp-server/main/config.example.yaml
```

(From a clone, that's `cp config.example.yaml config.yaml`.) Edit `database`
to match step 1 — host, port, `username`, `dbname`. Then trim
`masking.mask` to columns your schema actually has; it ships as a starter list
because forgetting one is the failure mode. Everything else has a working
default, and [Configure](#configure) documents the rest.

**4. Run it.**

```bash
export MYSQL_MCP_AUTH_TOKEN="$(openssl rand -hex 32)"
echo "$MYSQL_MCP_AUTH_TOKEN"          # your client needs this in step 5
export MYSQL_PASSWORD='a-strong-password'
mysql-mcp-server --config ./config.yaml
```

It stays in the foreground, and a healthy start logs one line:

```json
{"time":"...","level":"INFO","msg":"startup","listen":"127.0.0.1:3000","database":"127.0.0.1","mode":"read_only","masking":true,"version":"0.0.2"}
```

If it exits instead, the error says why — [Troubleshooting](#troubleshooting) has
the common ones.

**5. Ask it something.** In a second terminal, with `TOKEN` set to what step 4
printed:

```bash
curl -s -X POST http://127.0.0.1:3000/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":{"sql":"SHOW TABLES"}}}'
```

The reply is a server-sent-event `data:` line carrying the JSON-RPC result. If
curl gets rows back, an MCP client will too — wire one up under
[Connect a Client](#connect-a-client), then move the server to a host under
[Deploy](#deploy).

## Safety Model

- **Two modes, chosen per deployment.** `read_only` (the default, and what the
  rest of this list assumes) blocks all writes at the session level;
  `full_access` removes that block for environments where agent writes are
  wanted — there the user's grants are the only fence, so scope them
  deliberately and point production at `read_only` always.
- **Read-only, enforced by the database** — connect with a `SELECT`-only user;
  every pooled connection also runs `SET SESSION TRANSACTION READ ONLY`. The SQL
  text is never inspected — grants are the fence.
- **PII masking (optional)** — values of configured columns come back as
  `"<masked>"`. This is hygiene for cooperative callers — keeping personal data
  out of agent transcripts — **not** an airtight boundary. When it is on, it is
  enforced: the server reads every query and traces each result column back to
  its real source, so a personal column stays masked through renames,
  sub-queries, CTEs, and `UNION`s, and a computed column built from a personal
  one (`CONCAT(phone)`, `GROUP_CONCAT(phone)`, `MAX(phone)`) is masked too (a
  plain `COUNT` is a number and passes). A query it can't verify — a `SELECT *`
  inside a sub-query/join/union, or syntax it can't parse — is **refused** with
  a message telling the agent to simplify it.
  - **`masking.values`** adds a second layer that works by shape instead of by
    name: every text cell the column rules left alone is scanned for an email
    address or an Indonesian phone number, and the matching span comes back as
    `"<masked>"` while the rest of the cell stays readable. It catches personal
    data in places no column rule can name — free text, JSON blobs, a view's
    renamed column, a table copied under `full_access` — and it only ever masks
    more, never less. It is best-effort by nature: a name, an address, or a date
    has no shape, so the column rules stay the primary mechanism, and an
    `except` rule shields a plain column from both layers.
  - **JSON cells** get the column rules one level in: inside a cell that holds
    a JSON document (an activity log's `properties`, a request dump), a key
    named like a bare `mask` entry (`phone`, `address`, `*_email`) has its
    whole value returned as `"<masked>"`, nested objects included, and
    `masking.values` scans the strings that remain. There is no switch for it;
    it follows from the rules. So a bare rule for a word business data shares
    over-masks — a bare `name` would hide a room's name in its log entry as
    readily as a tenant's, which is why the starter list has no such rule.
    Qualify one (`users.name`) to keep it a column rule that never reaches
    keys; an `except` on the column itself (`activity_log.properties`) is the
    other carve-out, and shields the cell from every layer.
  - Still not a wall against a determined caller. Known gaps, documented by
    design (for those, use database-level controls — e.g. a user restricted to
    redacted views):
    - **views** — the server does not read view definitions, so a personal column
      exposed through a view is only masked if you add the view's column to the
      rules (e.g. `contact`) — or if `masking.values` recognises its shape;
    - values can still be **inferred** without ever appearing in the output —
      through a `WHERE` condition (`WHERE phone LIKE '0812%'`), or a window
      function's `PARTITION BY` / `ORDER BY` over a personal column (which reveals
      ordering or uniqueness, not the value) — neither of which masking inspects;
      and with `masking.values`, a cell that is masked only where a match sits
      reveals which rows held one;
    - **stored functions** that return personal data from inside their body;
    - **MariaDB-only syntax** the (MySQL-dialect) parser can't read is refused
      rather than run;
    - under **`full_access`**, masking degrades further — a write can copy
      personal data into tables the rules don't name (only `masking.values` can
      still catch it there, and only by shape), and writes, DDL, and
      unparseable statements run with wire-metadata masking only. This follows
      from the mode, so there is nothing to switch on; the server logs a
      warning at startup whenever masking runs alongside write access;
    - the server log carries the query text and the database's error text.
      With `masking.values` on, an email or phone shape in either is logged as
      `<masked>`, but a name, an address, or any other shapeless value still
      reaches the log — and with file logging it persists on disk, so protect
      log files like the data they describe.
- **Response cap** (default 500 KB of result JSON, ~125K tokens) — rows stream
  in and stop once the cap is hit; the response says so and hints to narrow the
  query. The raw HTTP body is roughly double the cap, because MCP encodes tool
  results twice.
- **Timeout** (default 30s) — the query is killed server-side with a
  `KILL QUERY` from a separate connection, with the engine's own statement
  timeout as backup (MariaDB `max_statement_time` / MySQL `max_execution_time`).
  One asymmetry: MySQL's variable only covers `SELECT`s, so under `full_access`
  on MySQL a long-running write is stopped by the `KILL` alone; MariaDB's
  covers every statement except stored procedures.
- **Connection pool** (default 10) — doubles as the concurrency brake.
- **Bearer token** — checked on every request, compared in constant time. The
  only exceptions are the two health probes, which reveal up or down and nothing
  else (see [Deploy](#deploy)). Under `--stdio` there is no token: the caller is
  whoever launched the process, and the operating system decides who can (see
  [Stdio](#stdio)).

## Requirements

- A reachable MySQL or MariaDB
- A host to run it on, near the database, plus a reverse proxy to terminate TLS
  in front of it. The [Quickstart](#quickstart) skips both and runs on your
  laptop; [Deploy](#deploy) covers them.
- Go 1.26 or newer, only if you build from source
- A `SELECT`-only database user (or, for `full_access`, a user whose grants say
  exactly what the agent may do) — [Quickstart](#quickstart) has the `GRANT`,
  and `seed/seed.sql` a throwaway database to try it against

Day-to-day development runs against MariaDB 10.11; MySQL is supported and the
suite accommodates both, so open an issue if a real MySQL 8 deployment disagrees.

## Install

Grab a prebuilt binary (Linux and macOS, amd64 and arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/mshddev/mysql-mcp-server/main/install.sh | sh
```

It picks the latest release, verifies the sha256 against the release's
`checksums.txt`, and puts `mysql-mcp-server` in `/usr/local/bin` if that's
writable, otherwise `~/.local/bin`. It never calls `sudo`. `VERSION=v0.0.2`
pins a release and `INSTALL_DIR=...` changes the destination. Piping a script
into `sh` is a trust decision; [`install.sh`](install.sh) is short, so read it
first if you'd rather.

Archives for every platform, including Windows, are on the
[Releases page](https://github.com/mshddev/mysql-mcp-server/releases) if you'd
rather download by hand.

With a Go toolchain, install straight into `$GOBIN`:

```bash
go install github.com/mshddev/mysql-mcp-server@latest
```

Or build from source:

```bash
git clone https://github.com/mshddev/mysql-mcp-server.git
cd mysql-mcp-server
go build -o mysql-mcp-server .
```

Whichever route you took, verify:

```bash
mysql-mcp-server --version
```

Release binaries and `go install` builds report their tag. A local build reports
the nearest tag with a `+dirty` or pseudo-version suffix.

## Configure

Copy the example and adjust it for your database:

```bash
cp config.example.yaml config.yaml
```

`config.yaml` is gitignored. `${VAR}` placeholders are pulled from the
environment, so nothing sensitive lands in the file. They work in `listen`,
`auth_token`, `host`, `username`, `password`, `dbname`, the `tls` paths and
`logging.file`; an unset variable is a startup error naming it, never a silent
empty string:

```yaml
mode: read_only                # or full_access: writes allowed, grants are the fence

server:
  listen: "127.0.0.1:3000"     # loopback on purpose; a reverse proxy fronts it (see Deploy)
  auth_token: ${MYSQL_MCP_AUTH_TOKEN}

database:
  host: 127.0.0.1
  port: 3306
  username: mcp_readonly
  password: ${MYSQL_PASSWORD}
  dbname: mcp_dev
  # tls:                       # optional; see Encrypting the database hop
  #   ca: /etc/mysql-mcp-server/db-ca.pem

limits:
  timeout_seconds: 30
  max_response_bytes: 512000
  max_connections: 10

logging:
  output: stdout        # or "file" with a `file:` path and optional `rotation:`
  level: info

masking:                # optional; omit the section to run without masking
  enabled: true
  mask: [phone, "*_phone", email, address]
  except: ["room_types.display_name"]
  values: [email, phone_id]   # optional second layer: mask by shape, not name
```

| Key | Meaning |
|---|---|
| `mode` | `read_only` (default) or `full_access`. See [Safety Model](#safety-model). |
| `server.listen` | Address to bind. Loopback by default, with a TLS-terminating proxy in front. A non-loopback bind warns at startup. Ignored under `--stdio`. |
| `server.auth_token` | Bearer token clients must present. Ignored under `--stdio`, placeholder included, so one file serves both transports. |
| `database.host` / `port` | Where the database lives. |
| `database.username` / `password` | The database user and its password. Read-only under `read_only`; under `full_access` its grants are the write fence. |
| `database.dbname` | Default database (schema) to connect to. |
| `database.tls` | Encrypt the connection to the database. Absent means plaintext. See [Encrypting the database hop](#encrypting-the-database-hop). |
| `database.tls.enabled` | Kill-switch. Defaults to true when any other `tls` key is present. |
| `database.tls.ca` | PEM file with the CA that signed the database's certificate. Omit to trust the system store, which covers public CAs. |
| `database.tls.server_name` | Name to verify the certificate against when it isn't `database.host` — a certificate for `db.internal` reached over an IP, say. |
| `database.tls.cert` / `key` | Client certificate and private key for mutual TLS, for a user granted with `REQUIRE X509`. Both or neither. |
| `database.tls.insecure_skip_verify` | Encrypt without checking who answers. Warns at startup; excludes `ca` and `server_name`. |
| `limits.timeout_seconds` | Per-query timeout before a server-side kill. |
| `limits.max_response_bytes` | Result-size cap before truncation. |
| `limits.max_connections` | Pool size, doubling as the concurrency ceiling. |
| `logging.output` | `stdout` (default) or `file`. Under `--stdio` the default writes to stderr, since stdout is the wire. |
| `logging.file` | Log file path; required with `output: file`. |
| `logging.level` | `debug`, `info` (default), `warn`, or `error`. |
| `logging.rotation` | For file output: `max_size_mb` (rotate at this size, default 100), `max_backups` / `max_age_days` (0 = keep everything, the default), `compress`. |
| `masking.enabled` | Kill-switch. Defaults to true when rules are present. |
| `masking.mask` | Case-insensitive globs of column names to mask — bare (`phone`) matches every table, qualified (`users.address`) just one. |
| `masking.except` | Carve-outs for false positives; beats `mask` and shields the column from `values` too. |
| `masking.values` | Shape detectors to run over text cells the column rules left alone: `email`, `phone_id` (Indonesian mobile numbers). Omit for none. |

Masking strictness is not configurable — it follows `mode`. Under `read_only`
every query is enforced. Under `full_access` reads the server can parse are
still enforced, while writes, DDL, and unparseable statements fall back to
wire-metadata masking; it warns at startup when it starts in that state.

### Encrypting the database hop

The server talks to MySQL in plaintext unless you say otherwise, and for the
deployment this README describes — the server on the same host or private
network as the database — that is the right default. Turn TLS on when the hop
crosses a network you don't control: a managed database (RDS, Cloud SQL,
PlanetScale), or a [`--stdio`](#stdio) server on a laptop reaching a database
somewhere else.

Two things to know before you do. **The database has to offer TLS.** The
driver has no "try TLS, then plaintext" mode, so a database without it is a
refused connection at startup, never a silent downgrade. MySQL 8 and MariaDB
11.4 turn it on by themselves at first start; older MariaDB needs `ssl_cert`,
`ssl_key` and `ssl_ca` set on the server. And **with TLS on, the certificate
is verified** the way a
browser verifies a website's, unless you explicitly opt out — which brings us
to the three shapes this takes.

**A managed database.** The provider publishes the CA that signed its
certificates; download it to the server's host and point at it:

```yaml
database:
  host: mydb.abc123.ap-southeast-1.rds.amazonaws.com
  tls:
    ca: /etc/mysql-mcp-server/rds-global-bundle.pem
```

Providers behind a public CA (PlanetScale, for one) need no `ca` at all:
`tls: {enabled: true}` verifies against the system trust store. If you reach
the database by an alias or an IP the certificate doesn't carry, add
`server_name` with the name it does carry.

**A self-hosted database with its own certificate.** Same as above, with the
`ca.pem` that signed the server's certificate copied over. One catch: the
certificate MySQL and MariaDB generate for themselves has no Subject
Alternative Name, only a placeholder Common Name, and Go refuses to verify a
certificate without a SAN no matter what `server_name` says. So for an
auto-generated certificate the choice is to issue a real one — `seed/tls/gen.sh`
shows the `openssl` incantation, SANs included — or to settle for encryption
without identity:

```yaml
  tls:
    insecure_skip_verify: true   # resists eavesdropping, not impersonation
```

The server warns at startup in that state, and the config refuses `ca` or
`server_name` alongside it, since neither would be checked.

**Mutual TLS.** When the database user is granted with `REQUIRE X509`, the
server has to present a client certificate the database trusts. Add the pair,
which the config expands `${VAR}` placeholders in like any other path:

```yaml
  tls:
    ca: /etc/mysql-mcp-server/db-ca.pem
    cert: /etc/mysql-mcp-server/client.pem
    key: /etc/mysql-mcp-server/client-key.pem
```

That is rarely worth the certificate management unless your team already runs
a private CA; the bearer token in front and a verified TLS hop behind cover
the same ground for most deployments.

The startup line reports `"tls":true` when the hop is encrypted, and
`SHOW SESSION STATUS LIKE 'Ssl_cipher'` through the `query` tool shows the
cipher from the database's side.

`config.example.yaml` ships a starter `mask` list to trim, not a blank page —
forgetting a column is the failure mode. A `masking` section that is enabled
but has neither `mask` rules nor `values` refuses to start; opt out explicitly
with `enabled: false` or by omitting the section.

Startup fails fast if the database is unreachable or a referenced env var is
unset.

## Run

```bash
export MYSQL_MCP_AUTH_TOKEN=...   # token clients must present
export MYSQL_PASSWORD=...         # password of the DB user
mysql-mcp-server --config ./config.yaml
```

Logs are one JSON line per query — time, request id, SQL, duration, truncated
flag, error if any. Results are never logged. They go to stdout by default;
`logging.output: file` writes them to a log file instead, rotated by size with
configurable retention (see `config.example.yaml`). A log file that can't be
created or written fails startup rather than running silent.

### Stdio

`--stdio` serves MCP over the process's own stdin and stdout instead of
listening for HTTP. The client launches the binary itself and speaks to it
over the pipes, which is the shape of a single person working against a
database on their own machine:

```bash
MYSQL_PASSWORD=... mysql-mcp-server --stdio --config /absolute/path/config.yaml
```

What changes under stdio:

- **No token.** Whoever can launch the process is the caller, so the `server`
  section is not read at all: `listen`, `auth_token`, and any `${VAR}` in them
  are ignored, and a config written for the HTTP deployment loads unchanged.
  The boundary is the operating system — whoever can run the binary and read
  its config can query the database as its user, so keep the database user
  read-only and the config file private.
- **Logs go to stderr**, because stdout is the wire. `logging.output: stdout`
  means stderr under `--stdio`, and `file` works as before.
- **No health probes.** There is nothing listening.
- **The database may be far away.** A laptop reaching a database on another
  network is the one case where the hop from this server to the database
  crosses something you don't control. Either an SSH tunnel
  (`ssh -N -L 3307:127.0.0.1:3306 db-host`, then `host: 127.0.0.1` and
  `port: 3307`) or `database.tls` — see
  [Encrypting the database hop](#encrypting-the-database-hop).
- **One session per process.** The server exits 0 when the client closes the
  pipe, and each client launch is a fresh process with its own pool.

The client has to keep stdin open for the session, as every MCP client does; a
one-shot pipe that closes as soon as it has written its requests gets no
answers. To try it from a shell, hold the pipe open for a moment:

```bash
(printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"shell","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query","arguments":{"sql":"SELECT 1"}}}'; sleep 1) \
  | mysql-mcp-server --stdio --config ./config.yaml
```

## Deploy

One instance, on a host near the database, shared by everyone's agents. The
binary binds to loopback and speaks plain HTTP, so a deployment has three
parts: the binary, a reverse proxy on the same host terminating TLS in front of
it, and a firewall that lets only the proxy's port through.

**1. Install on the host.** The installer from [Install](#install) works there
too, run as root so the binary lands in `/usr/local/bin`, where the unit below
expects it (it never calls `sudo` itself, and would otherwise fall back to
`~/.local/bin`). Then give the service its own user, put the config in place,
and keep the two secrets in an env file only that user can read:

```bash
curl -fsSL https://raw.githubusercontent.com/mshddev/mysql-mcp-server/main/install.sh | sudo sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin mysql-mcp
sudo install -d -m 750 -o root -g mysql-mcp /etc/mysql-mcp-server
sudo curl -fsSL -o /etc/mysql-mcp-server/config.yaml \
  https://raw.githubusercontent.com/mshddev/mysql-mcp-server/main/config.example.yaml
sudo tee /etc/mysql-mcp-server/env > /dev/null <<EOF
MYSQL_MCP_AUTH_TOKEN=$(openssl rand -hex 32)
MYSQL_PASSWORD=a-strong-password
EOF
sudo chmod 640 /etc/mysql-mcp-server/env
sudo chgrp mysql-mcp /etc/mysql-mcp-server/env
```

Edit `config.yaml` as in the Quickstart. Leave `listen` at `127.0.0.1:3000`;
the proxy is what faces the network.

On the database, cap the user's connections just above `limits.max_connections`
(the pool, 10 by default):

```sql
ALTER USER 'mcp_readonly'@'%' WITH MAX_USER_CONNECTIONS 12;
```

The pool is the server's own brake; this one is the database's, and it holds
even if the server misbehaves. Leave room for the kill connection each
timeout opens, which is why this is above the pool size rather than equal to
it. A second instance on the same user would hit the cap and be refused
connections, which is the point: give each instance its own user.

**2. Run it under systemd.** Save as `/etc/systemd/system/mysql-mcp-server.service`:

```ini
[Unit]
Description=mysql-mcp-server
After=network-online.target
Wants=network-online.target

[Service]
User=mysql-mcp
EnvironmentFile=/etc/mysql-mcp-server/env
ExecStart=/usr/local/bin/mysql-mcp-server --config /etc/mysql-mcp-server/config.yaml
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
# Uncomment with logging.output: file, and match logging.file's directory.
# ReadWritePaths=/var/log/mysql-mcp-server

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mysql-mcp-server
```

verify:

```bash
journalctl -u mysql-mcp-server -n 20
```

The `startup` line from the Quickstart is in there. With `logging.output:
stdout` (the default) journald keeps the query log; `journalctl -u
mysql-mcp-server -f` follows it. If the unit is restart-looping instead, the
error is in the same place: startup refuses to listen until the config resolves
and the database answers, and `Restart=on-failure` keeps retrying every five
seconds. A `systemctl restart` or `stop` is orderly: the server stops taking
connections, lets the queries already running finish (bounded by
`limits.timeout_seconds` plus a few seconds, and always under systemd's 90s
kill), hangs up its database connections, and exits 0.

**3. Terminate TLS in front.** Any reverse proxy works. With
[Caddy](https://caddyserver.com), the whole `Caddyfile` is:

```
mysql-mcp.internal.example.com {
	request_header X-Request-Id {http.request.uuid}
	reverse_proxy 127.0.0.1:3000
	log
}
```

Caddy fetches a public certificate on its own when the name resolves publicly.
For a name that only resolves inside your network, either hand it your own
certificate with `tls cert.pem key.pem`, or add `tls internal` and install
Caddy's root CA on every laptop that will connect.

The `request_header` line gives each request an id: Caddy sends it upstream as
`X-Request-Id`, which the server logs as `request_id` on the query line, and
records the same value as `uuid` in its access log, the one the `log` line
turns on (Caddy 2.8 or later for the field).

Two things any proxy has to get right. Its upstream timeout must exceed
`limits.timeout_seconds` with room to spare, or a slow query comes back as a
`504` instead of a result. And it must not buffer `text/event-stream`
responses. Caddy does both out of the box; nginx needs `proxy_read_timeout`
raised and `proxy_buffering off`, and for the request id
`proxy_set_header X-Request-Id $request_id;` plus `$request_id` in its
`log_format`.

**4. Open only the proxy's port.** `443` in, from wherever the agents run.
`3000` stays on loopback and never appears in a firewall rule.

If the host is already on a private network you trust (a VPC, a VPN, a
Tailscale tailnet), you can skip the proxy: set `listen: ":3000"` and let
clients use `http://` on that network. The token then crosses that network in
the clear, so make that call deliberately. The server logs a warning at
every start in that state, so the choice stays visible.

**5. Hand out the URL and the token.** The token is the one step 1 wrote to
`/etc/mysql-mcp-server/env`. Everyone gets the same two values, and
[Connect a Client](#connect-a-client) shows where they go. Rotating the token
is editing the env file and restarting the unit. There is no overlap window:
the old value stops working the moment the unit restarts, so hand out the new
one first.

verify, from a laptop:

```bash
curl -s https://mysql-mcp.internal.example.com/readyz
```

`{"status":"ok"}` means the whole path works: proxy, TLS, server, database.
Add the token and run the Quickstart's `tools/call` curl against the same host
to prove the last step too.

**6. Prove the fences before the team connects.** A config copied from staging
is how production ends up writable. Run each of these through the Quickstart's
`tools/call` curl against the production URL, and stop if any one of them
returns data instead of an error:

```sql
UPDATE users SET name = 'x' WHERE id = 1;     -- an error, not affected_rows
SELECT phone FROM users LIMIT 1;              -- "<masked>", named in masked_columns
SELECT * FROM (SELECT * FROM users) t;        -- refused: masking can't verify it
```

Then the same curl with no `Authorization` header, which must get `401`. Repeat
the list after every upgrade or config change; it takes a minute and it is the
only proof that the guards you think are on actually are.

**Where things are recorded.** Three logs, each holding a different part of
the story. The server log (journald, with the default `logging.output`) has
every statement with its duration and error, verbatim by default and with
email and phone shapes scrubbed when `masking.values` is on, and never the
rows. The proxy's access log has the caller's address, status, and timing, but
not the SQL, which travels in the request body. With the proxy stamping ids as
in step 3, the server line's `request_id` is the id in the proxy's access log,
so one call can be followed across both. The database's own general log or
audit plugin is the only record on the database's side, and it contains the
values from every `WHERE` clause, so protect it like the tables it describes.
None of the three names a person: one token and one database user serve the
whole team, so the trail ends at "someone with the token". Per-person tokens
are the planned fix.

**Health checks.** Two paths answer a bare `GET` (or `HEAD`) with no token,
which is what load balancers, container probes, and uptime monitors send:

| Path | Answers | Use it for |
|---|---|---|
| `/healthz` | `200 {"status":"ok"}` as long as the process serves HTTP. Never touches the database. | Liveness: restart the process if this fails. |
| `/readyz` | `200 {"status":"ok"}` when a database ping succeeds, `503 {"status":"degraded"}` when it doesn't. | Readiness: route traffic or page someone. |

Startup already refuses to run without the database, so `/readyz` is for the
database going away later; the server stays up and reports it here while every
query fails. A result is cached for five seconds, so probing it in a loop costs
the database one ping per five seconds no matter how many probers there are.
The body says up or down and nothing else; the reason is in the server log as a
`readiness` warning. Both paths return `405` to any other method, and every
other path still requires the token.

## Connect a Client

Every agent points at the same server. A client needs three things:

- **Endpoint** — the URL your deployment answers on, such as
  `https://mysql-mcp.internal.example.com/mcp`, or `http://127.0.0.1:3000/mcp`
  for the Quickstart trial (any path works except the two health probes;
  `/mcp` is the convention)
- **Header** — `Authorization: Bearer <your token>`
- **Tool** — `query`, one string argument, `sql`

The transport is streamable HTTP. A client that can only launch subprocesses
can run its own copy with `--stdio` instead (see [Stdio](#stdio)); that is a
per-person server, not a way to reach the shared one.

The server is stateless, so there is no session handshake to do first: a client
can call `tools/list` or `tools/call` cold, which is also why the curl in
[Quickstart](#quickstart) works on its own.

### Claude Code

Point `.mcp.json` at the server, keeping the token out of git with `${VAR}`
expansion:

```json
{
  "mcpServers": {
    "db-staging": {
      "url": "https://mysql-mcp.internal.example.com/mcp",
      "type": "http",
      "headers": {
        "Authorization": "Bearer ${MYSQL_MCP_AUTH_TOKEN}"
      }
    }
  }
}
```

Name the entry for the environment it reaches — `db-staging`,
`db-prod-readonly` — not `mysql`. The agent picks a server by its name, and a
vague one is how it ends up querying the wrong database once two are
configured.

Commit that file and everyone on the repo gets the same server; the token comes
from each person's shell. Export `MYSQL_MCP_AUTH_TOKEN` in your shell profile —
the same value the server runs with — then restart Claude Code and ask a data
question. The agent will use `SHOW TABLES` / `DESCRIBE` to find its way around,
then `SELECT`.

For a production server, make every query ask first. In the project's
`.claude/settings.json`:

```json
{
  "permissions": {
    "ask": ["mcp__db-prod-readonly__query"]
  }
}
```

The name is `mcp__<server>__<tool>`. Each call to that server now stops for a
person to approve before it runs, while staging stays hands-off. It is cheap
insurance on top of the read-only user: the agent still can't write, but a
`SELECT` that scans a large table on production is worth a glance too.

For a local database, let Claude Code launch the server itself over stdio. Use
an absolute config path: the subprocess starts in the project root, not next
to the binary.

```json
{
  "mcpServers": {
    "mysql": {
      "command": "mysql-mcp-server",
      "args": ["--stdio", "--config", "/home/you/.config/mysql-mcp-server/config.yaml"],
      "env": {
        "MYSQL_PASSWORD": "${MYSQL_PASSWORD}"
      }
    }
  }
}
```

### Other clients

Any client that speaks streamable HTTP and can set a header takes the same three
values. If yours can't set one, put a proxy in front that adds it: the token is
checked on every request, and it is the only way in. A client that only speaks
stdio launches `mysql-mcp-server --stdio --config <path>` as its command.

## The `query` Tool

One tool, one argument:

```
query(sql: string)
```

Rows come back as JSON objects keyed by the column label as written in the
query:

```json
{
  "columns": ["id", "name", "phone"],
  "rows": [
    {"id": 1, "name": "Andi", "phone": "0812..."},
    {"id": 2, "name": "Budi", "phone": null}
  ],
  "truncated": false
}
```

A few rules worth knowing:

- Duplicate labels — `SELECT u.id, b.id` on a join — are qualified with the
  table alias (`u.id`, `b.id`); with no table to qualify by, they get a numeric
  suffix (`x`, `x_2`). `columns` lists the keys once, in SELECT order.
- `NULL` is JSON `null`. Binary cells become `"<binary, N bytes>"`.
- Columns caught by the server's PII policy come back as `"<masked>"` (their
  `NULL`s stay `null`); the response names them in `masked_columns` and the
  `note` says why, so the agent won't mistake the placeholder for data.
- With `masking.values` on, text inside an otherwise real cell that looks like
  an email address or a phone number becomes `"<masked>"` in place;
  `masked_values` maps each affected column to the detectors that fired, and
  the `note` says so.
- A cell holding a JSON document keeps its shape, but a key named like a
  masked column has its value replaced by `"<masked>"`; `masked_json_keys`
  maps each affected column to those key names, and the `note` says so. A
  cell nothing matched comes back byte for byte as stored.
- `DECIMAL` stays a string to keep precision, and so do integers past ±2^53
  (`BIGINT` IDs) — the MCP SDK round-trips numbers through a float64, which would
  otherwise corrupt them.
- MySQL errors pass through verbatim, so the agent can read them and
  self-correct.
- Under `full_access`, a statement that returns no rows (INSERT/UPDATE/DELETE/
  DDL) reports `affected_rows` — present even at 0 — and `last_insert_id` when
  there is one. Each call is one statement with autocommit: no semicolon
  batches, no transactions spanning calls, and session state (`SET ...`) does
  not persist between calls.

## Troubleshooting

Startup problems are loud on purpose — the server refuses to listen until the
config resolves and the database answers.

| What you see | What it means |
|---|---|
| `config references unset environment variables: [MYSQL_MCP_AUTH_TOKEN]` | A `${VAR}` in the config has nothing behind it. Export it, or write the literal value in if it isn't a secret. |
| `database unreachable: dial tcp …: connect: connection refused` | Wrong host or port, or the database is down. |
| `database login refused: … ERROR 1045 (28000): Access denied for user …` | Wrong `MYSQL_PASSWORD`, or the user doesn't exist for the host you connect *from*. A default MariaDB install keeps an anonymous `''@'localhost'` that shadows `'user'@'%'` on local connections, so create the `@'localhost'` variant too. |
| `database login refused: … ERROR 1044 (42000): Access denied for user … to database …` | The user has no grant on `database.dbname` — misspelled, or the `GRANT` named a different schema. |
| `database does not offer TLS but database.tls is on: …` | The database isn't serving TLS. MySQL 8 and MariaDB 11.4 do by default; older MariaDB needs `ssl_cert`, `ssl_key` and `ssl_ca` set server-side. Or drop the `tls` section if the hop is local. |
| `database requires TLS (set database.tls.enabled: true): …` | The database has `require_secure_transport=ON` and refuses plaintext. Add a `tls` section. |
| `database certificate is signed by a CA this server doesn't trust …` | `database.tls.ca` is missing or points at the wrong file. Use the CA that signed the database's certificate, not the certificate itself. |
| `database certificate is not for this host …` | The certificate's names don't include `database.host`. Set `database.tls.server_name` to one they do include. If the error mentions the legacy Common Name, the certificate has no SAN at all — MySQL's auto-generated one, typically — and only `insecure_skip_verify` or a real certificate will do. |
| `database login refused: … Access denied` with `tls` on and the user granted `REQUIRE SSL` or `REQUIRE X509` | The database enforces the requirement at login, and a missing client certificate looks exactly like a bad password. Check `SHOW CREATE USER` and add `cert` / `key` if it says `X509`. |
| `create log directory: mkdir …: read-only file system` | `logging.output: file` pointing somewhere it can't write. The server creates the directory when it can, and fails startup when it can't, rather than running silent. |
| `401 unauthorized` on every call | Token mismatch. Compare what the client sends with `MYSQL_MCP_AUTH_TOKEN`, and check the header reads `Authorization: Bearer <token>`. |
| `405 Method Not Allowed` | You sent a `GET` to the MCP endpoint, or a `POST` to a health probe. MCP calls are `POST`; `/healthz` and `/readyz` are `GET`. |
| `/readyz` says `degraded` | The database stopped answering after startup. The `readiness` warning in the server log has the driver's error. |
| `502` or `504` from the proxy | `502`: the service is down, so check `systemctl status mysql-mcp-server`. `504`: the proxy's upstream timeout is shorter than `limits.timeout_seconds`. |
| `curl: (60) SSL certificate problem` | The proxy is using a certificate your machine doesn't trust — `tls internal` or self-signed. Install its root CA, or give the proxy a certificate from a CA you already trust. |
| `ERROR 1142 (42000): … command denied to user …` | Read-only doing its job: the grants refused a write. |
| `PII masking refused this query: only SELECT/SHOW/DESCRIBE/EXPLAIN are allowed in read_only mode (this is a DELETE)` | The same refusal one layer earlier — with masking on, the parser stops a write before the database sees it. |
| `PII masking refused this query: a SELECT * inside a sub-query, join, or union can't be verified` | Masking can't trace `*` back to real columns. List them explicitly. |
| `PII masking refused this query: could not parse it to verify masking` | The MySQL-dialect parser couldn't read the statement, usually MariaDB-only syntax. Rewrite it, or run that deployment without masking. |
| A column comes back `"<masked>"` and shouldn't | A rule matched its name. Put the qualified column in `masking.except` — it beats `mask`. |
| A column you wanted masked comes back in the clear | Nothing matched it. Rules match a column's *real* name, so a view that renames one needs the view's own column added — or, for emails and phone numbers, `masking.values`. See the views gap in [Safety Model](#safety-model). |
| Part of a value comes back `"<masked>"` inside otherwise real text | A `masking.values` detector matched its shape — an invoice number that looks like a phone, say. Put the qualified column in `masking.except`, which shields it from both layers, or drop that detector. |
| A key inside a JSON cell comes back `"<masked>"` and shouldn't | A bare `mask` entry names that key (`name` inside a room's log entry, say); `masked_json_keys` says which. Put the qualified column (`activity_log.properties`) in `masking.except`, which shields the whole cell, or qualify the rule (`users.name`) so it stops reaching keys. |
| `truncated at ~N bytes — narrow the query (add WHERE or LIMIT)` | The response cap. Narrow the query, or raise `limits.max_response_bytes`. |
| Every query dies at the same duration | `limits.timeout_seconds`. The kill runs server-side, so the database stops working on it too. |
| Writes still fail under `full_access` | Grants are the only fence there. Check `SHOW GRANTS`, and confirm the startup line says `"mode":"full_access"`. |
| The agent keeps hitting refusals it can't fix | Masking strictness follows `mode` and can't be tuned. Either simplify the queries, name the columns, or run that deployment with `masking.enabled: false`. |

## Local Development

Start a throwaway database — a couple of tables, fake rows, a `SELECT`-only
user (`mcp_readonly`), a full-access user (`mcp_write`) for exercising
`full_access` mode, and a `REQUIRE X509` user (`mcp_x509`) for mutual TLS.
`compose.yaml` runs MariaDB in a container with `seed/seed.sql` applied and
TLS offered with the throwaway certificates in `seed/tls`:

```bash
docker compose up -d --wait      # or: podman compose up -d --wait
```

Or seed a MySQL/MariaDB you already have:

```bash
mysql -h 127.0.0.1 -u root < seed/seed.sql
```

Run against it:

```bash
MYSQL_MCP_AUTH_TOKEN=localsecret123 MYSQL_PASSWORD=devpassword ./mysql-mcp-server
```

Run the tests:

```bash
go test ./...
```

The unit tests always run. The integration tests are skipped unless
`MYSQL_TEST_ADDR` points at a seeded database:

```bash
MYSQL_TEST_ADDR=127.0.0.1:3306 go test ./...
```

The TLS tests also need `MYSQL_TEST_TLS_CA=seed/tls/ca.pem` and skip without
it. Override the credentials with `MYSQL_TEST_USER`, `MYSQL_TEST_PASSWORD` and
`MYSQL_TEST_DATABASE` if yours differ from the seed. CONTRIBUTING.md covers
re-seeding, changing the port, and running against MySQL instead of MariaDB.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the dev
setup, tests, and PR flow, and the [Code of Conduct](CODE_OF_CONDUCT.md) for the
ground rules.

## Security

Found a vulnerability? Report it privately — see [SECURITY.md](SECURITY.md). Do
not open a public issue for security problems.

## License

[MIT](LICENSE) © mshddev

If you find this useful, a star on GitHub is appreciated — and feel free to
contribute.
