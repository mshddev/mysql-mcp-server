# mysql-mcp-server

MCP server for MySQL/MariaDB. One tool — `query` — runs read-only
SQL over streamable HTTP (MCP spec 2026-07-28, stateless) and returns rows.

## Safety model

- **Read-only, enforced by the database**: connect with a `SELECT`-only user;
  every pooled connection also runs `SET SESSION TRANSACTION READ ONLY`.
  SQL text is never inspected — grants are the fence.
- **Response cap** (default 500 KB of result JSON, ~125K tokens of result
  text): rows stream in and streaming stops once the cap is hit; the response
  says so, with a hint to narrow the query. Note the raw HTTP response is
  roughly double the cap, because MCP encodes tool results twice (text +
  structured output).
- **Timeout** (default 30s): the query is killed server-side (`KILL QUERY`
  from a separate connection), with the engine's own statement timeout as
  backup (auto-detects MariaDB `max_statement_time` vs MySQL
  `max_execution_time`).
- **Connection pool** (default 10) doubles as the concurrency brake.
- **Bearer token** auth on every request, compared in constant time.

## Run

```sh
go build -o mysql-mcp-server .
cp config.example.yaml config.yaml   # then adjust host/database per environment
export MCP_AUTH_TOKEN=...     # token clients must present
export MYSQL_PASSWORD=...     # password of the read-only DB user
./mysql-mcp-server --config ./config.yaml
```

`config.yaml` is gitignored — only `config.example.yaml` ships. `${VAR}`
placeholders pull the two secrets from the environment, so nothing sensitive
lives in git. Startup fails fast if the database is unreachable or a
referenced env var is unset.

Logs are one JSON line per query on stdout: time, SQL, duration_ms,
truncated flag, error if any. Results are never logged.

## Local development

Seed the throwaway dev database (tables, fake rows, `mcp_readonly` user)
into a local MySQL/MariaDB:

```sh
mysql -h 127.0.0.1 -u root < seed/seed.sql
MCP_AUTH_TOKEN=localsecret123 MYSQL_PASSWORD=devpassword ./mysql-mcp-server
```

## Client setup (Claude Code)

`.mcp.json` — keep the real token out of git with `${VAR}` expansion:

```json
{
  "mcpServers": {
    "staging_mysql": {
      "url": "http://localhost:3000/mcp",
      "type": "http",
      "headers": {
        "Authorization": "Bearer ${STAGING_MYSQL_MCP_TOKEN}"
      }
    }
  }
}
```

Each teammate sets `STAGING_MYSQL_MCP_TOKEN` in their shell profile.

## Staging demo checklist

1. Edit `config.yaml` (or a copy): staging host/port, the existing
   `SELECT`-only staging user, staging database name.
2. `SHOW GRANTS` for that user — whatever it can read is what agents can read.
3. Start the server with the staging config and real secrets in env.
4. Point `.mcp.json` at it and ask Claude Code a data question.

## Response shape

```json
{
  "columns": ["id", "name", "phone"],
  "rows": [{"id": 1, "name": "Andi", "phone": "0812..."}, {"id": 2, "name": "Budi", "phone": null}],
  "truncated": false
}
```

Rows are objects keyed by the column label as written in the query (aliases
respected). Duplicate labels — `SELECT u.id, b.id` on a join — are qualified
with the table alias (`u.id`, `b.id`); if there is no table to qualify by,
they get a numeric suffix (`x`, `x_2`). `columns` lists the same keys once in
SELECT order, since JSON object keys serialize alphabetically. `NULL` is JSON
`null`. Binary cells become `"<binary, N bytes>"`. `DECIMAL` values stay
strings to keep precision, as do integers past ±2^53 (`BIGINT` IDs) — the MCP
SDK round-trips structured output through a float64, which would otherwise
corrupt them. MySQL errors pass through verbatim so agents can self-correct.
