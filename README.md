# mysql-mcp-server

> Read-only SQL access to MySQL/MariaDB for AI agents, over MCP.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](go.mod)

An MCP server that gives an AI agent a safe, read-only window into a MySQL or
MariaDB database. It exposes a single tool — `query` — over streamable HTTP
(MCP spec 2026-07-28, stateless), runs the SQL you send, and returns rows as
JSON objects.

The point is to let a coding agent (Claude Code, or anything that speaks MCP)
answer real data questions and explore a schema — without the risk of it
writing, dropping a table, or dragging a whole dataset across the wire.
Read-only is enforced by the database, not by parsing your SQL.

## Safety Model

- **Read-only, enforced by the database** — connect with a `SELECT`-only user;
  every pooled connection also runs `SET SESSION TRANSACTION READ ONLY`. The SQL
  text is never inspected — grants are the fence.
- **PII masking (optional)** — values of configured columns come back as
  `"<masked>"`, matched on the column's *origin* name from the wire protocol.
  A plain column keeps its origin through a rename (`phone AS x`), so simple
  aliases can't dodge it. This is hygiene for cooperative callers — keeping
  personal data out of agent transcripts — **not a security boundary**. Known
  ways masking does **not** apply (a caller that must never see the data needs
  database-level controls instead):
    - computed columns — `CONCAT(...)`, aggregates, `GROUP_CONCAT(col)` — have
      no origin and pass through;
    - **derived tables, CTEs and `UNION`** lose the base-column origin on the
      wire, so those columns currently pass through unmasked — a known gap, fix
      pending;
    - **any view** reports the view as the origin table, so a table-qualified
      rule (`users.address`) stops matching through a view; unqualified rules
      (`address`) still apply;
    - values can still be inferred with `WHERE` conditions, and query text in
      the server log is not scrubbed.
- **Response cap** (default 500 KB of result JSON, ~125K tokens) — rows stream
  in and stop once the cap is hit; the response says so and hints to narrow the
  query. The raw HTTP body is roughly double the cap, because MCP encodes tool
  results twice.
- **Timeout** (default 30s) — the query is killed server-side with a
  `KILL QUERY` from a separate connection, with the engine's own statement
  timeout as backup (MariaDB `max_statement_time` / MySQL `max_execution_time`).
- **Connection pool** (default 10) — doubles as the concurrency brake.
- **Bearer token** — checked on every request, compared in constant time.

## Requirements

- Go 1.26 or newer
- A reachable MySQL or MariaDB
- A `SELECT`-only database user — see [Local Development](#local-development) for
  a seed you can copy

## Install

Build from source:

```bash
git clone https://github.com/mshddev/mysql-mcp-server.git
cd mysql-mcp-server
go build -o mysql-mcp-server .
```

verify:

```bash
./mysql-mcp-server --help
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

`config.yaml` is gitignored. The two secrets are pulled from the environment via
`${VAR}` placeholders, so nothing sensitive lands in the file:

```yaml
server:
  listen: "127.0.0.1:3000"     # loopback by default; ":3000" exposes on all interfaces
  auth_token: ${MCP_AUTH_TOKEN}

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

masking:                # optional; omit the section to run without masking
  enabled: true
  mask: [phone, "*_phone", email, name, address]
  except: ["room_types.display_name"]
```

| Key | Meaning |
|---|---|
| `server.listen` | Address to bind. Loopback by default. |
| `server.auth_token` | Bearer token clients must present. |
| `database.host` / `port` | Where the database lives. |
| `database.username` / `password` | The read-only user and its password. |
| `database.dbname` | Default database (schema) to connect to. |
| `limits.timeout_seconds` | Per-query timeout before a server-side kill. |
| `limits.max_response_bytes` | Result-size cap before truncation. |
| `limits.max_connections` | Pool size, doubling as the concurrency ceiling. |
| `masking.enabled` | Kill-switch. Defaults to true when rules are present. |
| `masking.mask` | Case-insensitive globs of column names to mask — bare (`phone`) matches every table, qualified (`users.address`) just one. |
| `masking.except` | Carve-outs for false positives; beats `mask`. |

`config.example.yaml` ships a starter `mask` list to trim, not a blank page —
forgetting a column is the failure mode. A `masking` section that is enabled
but has no `mask` rules refuses to start; opt out explicitly with
`enabled: false` or by omitting the section.

Startup fails fast if the database is unreachable or a referenced env var is
unset.

## Run

```bash
export MCP_AUTH_TOKEN=...     # token clients must present
export MYSQL_PASSWORD=...     # password of the read-only DB user
./mysql-mcp-server --config ./config.yaml
```

Logs are one JSON line per query on stdout — time, SQL, duration, truncated
flag, error if any. Results are never logged.

## Connect a Client (Claude Code)

Point `.mcp.json` at the server, keeping the token out of git with `${VAR}`
expansion:

```json
{
  "mcpServers": {
    "mysql": {
      "url": "http://localhost:3000/mcp",
      "type": "http",
      "headers": {
        "Authorization": "Bearer ${MYSQL_MCP_TOKEN}"
      }
    }
  }
}
```

Set `MYSQL_MCP_TOKEN` in your shell profile to the same value as
`MCP_AUTH_TOKEN`. Then ask the agent a data question — it will use `SHOW TABLES`
/ `DESCRIBE` to find its way around, then `SELECT`.

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

## Local Development

Seed a throwaway database — a couple of tables, fake rows, and a `SELECT`-only
user — into a local MySQL/MariaDB:

```bash
mysql -h 127.0.0.1 -u root < seed/seed.sql
```

Run against it:

```bash
MCP_AUTH_TOKEN=localsecret123 MYSQL_PASSWORD=devpassword ./mysql-mcp-server
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
