# mysql-mcp-server

> SQL access to MySQL/MariaDB for AI agents over MCP — read-only by default,
> full access as an explicit opt-in.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](go.mod)

An MCP server that gives an AI agent a window into a MySQL or MariaDB
database. It exposes a single tool — `query` — over streamable HTTP
(MCP spec 2026-07-28, stateless), runs the SQL you send, and returns rows as
JSON objects.

The point is to let a coding agent (Claude Code, or anything that speaks MCP)
answer real data questions and explore a schema — without the risk of it
writing, dropping a table, or dragging a whole dataset across the wire. In the
default `read_only` mode that safety is enforced by the database, not by
parsing your SQL. For disposable environments like staging, `mode: full_access`
drops the server-side write block and lets the MySQL user's grants decide what
the agent may do — writes included.

## Quickstart

Zero to a working server against a database you already have. You need Go 1.26+,
a reachable MySQL or MariaDB, and enough access on it to create a user.

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

**2. Build it.**

```bash
git clone https://github.com/mshddev/mysql-mcp-server.git
cd mysql-mcp-server
go build -o mysql-mcp-server .
```

verify:

```bash
./mysql-mcp-server --version
```

**3. Write the config.**

```bash
cp config.example.yaml config.yaml
```

Edit `database` to match step 1 — host, port, `username`, `dbname`. Then trim
`masking.mask` to columns your schema actually has; it ships as a starter list
because forgetting one is the failure mode. Everything else has a working
default, and [Configure](#configure) documents the rest.

**4. Run it.**

```bash
export MYSQL_MCP_AUTH_TOKEN="$(openssl rand -hex 32)"
echo "$MYSQL_MCP_AUTH_TOKEN"          # your client needs this in step 5
export MYSQL_PASSWORD='a-strong-password'
./mysql-mcp-server --config ./config.yaml
```

It stays in the foreground, and a healthy start logs one line:

```json
{"time":"...","level":"INFO","msg":"startup","listen":"127.0.0.1:3000","database":"127.0.0.1","mode":"read_only","masking":true,"version":"0.0.1"}
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
[Connect a Client](#connect-a-client).

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
  - Still not a wall against a determined caller. Known gaps, documented by
    design (for those, use database-level controls — e.g. a user restricted to
    redacted views):
    - **views** — the server does not read view definitions, so a personal column
      exposed through a view is only masked if you add the view's column to the
      rules (e.g. `contact`);
    - values can still be **inferred** without ever appearing in the output —
      through a `WHERE` condition (`WHERE phone LIKE '0812%'`), or a window
      function's `PARTITION BY` / `ORDER BY` over a personal column (which reveals
      ordering or uniqueness, not the value) — neither of which masking inspects;
    - **stored functions** that return personal data from inside their body;
    - **MariaDB-only syntax** the (MySQL-dialect) parser can't read is refused
      rather than run;
    - under **`full_access`**, masking degrades further — a write can copy
      personal data into tables the rules don't name, and writes, DDL, and
      unparseable statements run with wire-metadata masking only. This follows
      from the mode, so there is nothing to switch on; the server logs a
      warning at startup whenever masking runs alongside write access;
    - query text in the server log is not scrubbed — and with file logging it
      persists on disk, so protect log files like the data they describe.
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
- **Bearer token** — checked on every request, compared in constant time.

## Requirements

- Go 1.26 or newer
- A reachable MySQL or MariaDB
- A `SELECT`-only database user (or, for `full_access`, a user whose grants say
  exactly what the agent may do) — [Quickstart](#quickstart) has the `GRANT`,
  and `seed/seed.sql` a throwaway database to try it against

Day-to-day development runs against MariaDB 10.11; MySQL is supported and the
suite accommodates both, so open an issue if a real MySQL 8 deployment disagrees.

## Install

Build from source:

```bash
git clone https://github.com/mshddev/mysql-mcp-server.git
cd mysql-mcp-server
go build -o mysql-mcp-server .
```

verify:

```bash
./mysql-mcp-server --version
```

Or install the binary straight into `$GOBIN`:

```bash
go install github.com/mshddev/mysql-mcp-server@latest
```

## Configure

Copy the example and adjust it for your database:

```bash
cp config.example.yaml config.yaml
```

`config.yaml` is gitignored. `${VAR}` placeholders are pulled from the
environment, so nothing sensitive lands in the file. They work in `listen`,
`auth_token`, `host`, `username`, `password`, `dbname` and `logging.file`; an
unset variable is a startup error naming it, never a silent empty string:

```yaml
mode: read_only                # or full_access: writes allowed, grants are the fence

server:
  listen: "127.0.0.1:3000"     # loopback by default; ":3000" exposes on all interfaces
  auth_token: ${MYSQL_MCP_AUTH_TOKEN}

database:
  host: 127.0.0.1
  port: 3306
  username: mcp_readonly
  password: ${MYSQL_PASSWORD}
  dbname: mcp_dev

limits:
  timeout_seconds: 30
  max_response_bytes: 512000
  max_connections: 10

logging:
  output: stdout        # or "file" with a `file:` path and optional `rotation:`
  level: info

masking:                # optional; omit the section to run without masking
  enabled: true
  mask: [phone, "*_phone", email, name, address]
  except: ["room_types.display_name"]
```

| Key | Meaning |
|---|---|
| `mode` | `read_only` (default) or `full_access`. See [Safety Model](#safety-model). |
| `server.listen` | Address to bind. Loopback by default. |
| `server.auth_token` | Bearer token clients must present. |
| `database.host` / `port` | Where the database lives. |
| `database.username` / `password` | The database user and its password. Read-only under `read_only`; under `full_access` its grants are the write fence. |
| `database.dbname` | Default database (schema) to connect to. |
| `limits.timeout_seconds` | Per-query timeout before a server-side kill. |
| `limits.max_response_bytes` | Result-size cap before truncation. |
| `limits.max_connections` | Pool size, doubling as the concurrency ceiling. |
| `logging.output` | `stdout` (default) or `file`. |
| `logging.file` | Log file path; required with `output: file`. |
| `logging.level` | `debug`, `info` (default), `warn`, or `error`. |
| `logging.rotation` | For file output: `max_size_mb` (rotate at this size, default 100), `max_backups` / `max_age_days` (0 = keep everything, the default), `compress`. |
| `masking.enabled` | Kill-switch. Defaults to true when rules are present. |
| `masking.mask` | Case-insensitive globs of column names to mask — bare (`phone`) matches every table, qualified (`users.address`) just one. |
| `masking.except` | Carve-outs for false positives; beats `mask`. |

Masking strictness is not configurable — it follows `mode`. Under `read_only`
every query is enforced. Under `full_access` reads the server can parse are
still enforced, while writes, DDL, and unparseable statements fall back to
wire-metadata masking; it warns at startup when it starts in that state.

`config.example.yaml` ships a starter `mask` list to trim, not a blank page —
forgetting a column is the failure mode. A `masking` section that is enabled
but has no `mask` rules refuses to start; opt out explicitly with
`enabled: false` or by omitting the section.

Startup fails fast if the database is unreachable or a referenced env var is
unset.

## Run

```bash
export MYSQL_MCP_AUTH_TOKEN=...   # token clients must present
export MYSQL_PASSWORD=...         # password of the DB user
./mysql-mcp-server --config ./config.yaml
```

Logs are one JSON line per query — time, SQL, duration, truncated flag, error
if any. Results are never logged. They go to stdout by default; `logging.output:
file` writes them to a log file instead, rotated by size with configurable
retention (see `config.example.yaml`). A log file that can't be created or
written fails startup rather than running silent.

## Connect a Client

The transport is **streamable HTTP only — there is no stdio mode**, so a client
that only launches subprocesses can't talk to this. Everything else needs three
things:

- **Endpoint** — `http://localhost:3000/mcp` (any path on the port works; `/mcp`
  is the convention)
- **Header** — `Authorization: Bearer <your token>`
- **Tool** — `query`, one string argument, `sql`

The server is stateless, so there is no session handshake to do first: a client
can call `tools/list` or `tools/call` cold, which is also why the curl in
[Quickstart](#quickstart) works on its own.

### Claude Code

Point `.mcp.json` at the server, keeping the token out of git with `${VAR}`
expansion:

```json
{
  "mcpServers": {
    "mysql": {
      "url": "http://localhost:3000/mcp",
      "type": "http",
      "headers": {
        "Authorization": "Bearer ${MYSQL_MCP_AUTH_TOKEN}"
      }
    }
  }
}
```

Export `MYSQL_MCP_AUTH_TOKEN` in your shell profile — the same value the server
runs with — then restart Claude Code and ask a data question. The agent will use
`SHOW TABLES` / `DESCRIBE` to find its way around, then `SELECT`.

### Other clients

Any client that speaks streamable HTTP and can set a header takes the same three
values. If yours can't set one, put a proxy in front that adds it: the token is
checked on every request, and it is the only way in.

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
| `create log directory: mkdir …: read-only file system` | `logging.output: file` pointing somewhere it can't write. The server creates the directory when it can, and fails startup when it can't, rather than running silent. |
| `401 unauthorized` on every call | Token mismatch. Compare what the client sends with `MYSQL_MCP_AUTH_TOKEN`, and check the header reads `Authorization: Bearer <token>`. |
| `405 Method Not Allowed` | You sent a `GET`. Every call is a `POST` — including the health check you were probably reaching for, which doesn't exist. |
| `ERROR 1142 (42000): … command denied to user …` | Read-only doing its job: the grants refused a write. |
| `PII masking refused this query: only SELECT/SHOW/DESCRIBE/EXPLAIN are allowed in read_only mode (this is a DELETE)` | The same refusal one layer earlier — with masking on, the parser stops a write before the database sees it. |
| `PII masking refused this query: a SELECT * inside a sub-query, join, or union can't be verified` | Masking can't trace `*` back to real columns. List them explicitly. |
| `PII masking refused this query: could not parse it to verify masking` | The MySQL-dialect parser couldn't read the statement, usually MariaDB-only syntax. Rewrite it, or run that deployment without masking. |
| A column comes back `"<masked>"` and shouldn't | A rule matched its name. Put the qualified column in `masking.except` — it beats `mask`. |
| A column you wanted masked comes back in the clear | Nothing matched it. Rules match a column's *real* name, so a view that renames one needs the view's own column added. See the views gap in [Safety Model](#safety-model). |
| `truncated at ~N bytes — narrow the query (add WHERE or LIMIT)` | The response cap. Narrow the query, or raise `limits.max_response_bytes`. |
| Every query dies at the same duration | `limits.timeout_seconds`. The kill runs server-side, so the database stops working on it too. |
| Writes still fail under `full_access` | Grants are the only fence there. Check `SHOW GRANTS`, and confirm the startup line says `"mode":"full_access"`. |
| The agent keeps hitting refusals it can't fix | Masking strictness follows `mode` and can't be tuned. Either simplify the queries, name the columns, or run that deployment with `masking.enabled: false`. |

## Local Development

Seed a throwaway database — a couple of tables, fake rows, a `SELECT`-only
user (`mcp_readonly`), and a full-access user (`mcp_write`) for exercising
`full_access` mode — into a local MySQL/MariaDB:

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

Override the credentials with `MYSQL_TEST_USER`, `MYSQL_TEST_PASSWORD` and
`MYSQL_TEST_DATABASE` if yours differ from the seed.

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
