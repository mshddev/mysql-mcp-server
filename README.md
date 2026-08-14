# mysql-mcp-server

Minimalist MCP server for MySQL/MariaDB. One tool — `query` — runs read-only
SQL over streamable HTTP (MCP spec 2026-07-28, stateless) and returns rows.

Built as the team replacement for the old `staging_mysql` MCP entry:
same client shape (URL + bearer token), centralized config/logs/auth.

## Safety model

- **Read-only, enforced by the database**: connect with a `SELECT`-only user;
  every pooled connection also runs `SET SESSION TRANSACTION READ ONLY`.
  SQL text is never inspected — grants are the fence.
- **Response cap** (default 500 KB, ~125K tokens): rows stream in and streaming stops once
  the cap is hit; the response says so, with a hint to narrow the query.
- **Timeout** (default 30s): the query is killed server-side (`KILL QUERY`
  from a separate connection), with the engine's own statement timeout as
  backup (auto-detects MariaDB `max_statement_time` vs MySQL
  `max_execution_time`).
- **Connection pool** (default 10) doubles as the concurrency brake.
- **Bearer token** auth on every request, compared in constant time.

## Run

```sh
go build -o mysql-mcp-server .
export MCP_AUTH_TOKEN=...     # token clients must present
export MYSQL_PASSWORD=...     # password of the read-only DB user
./mysql-mcp-server --config ./config.yaml
```

`config.yaml` holds structure and limits; `${VAR}` placeholders pull the two
secrets from the environment, so nothing sensitive lives in git. Startup
fails fast if the database is unreachable or a referenced env var is unset.

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
  "rows": [[1, "Andi", "0812..."], [2, "Budi", null]],
  "truncated": false
}
```

Column names appear once; rows are arrays. `NULL` is JSON `null`. Binary
cells become `"<binary, N bytes>"`. `DECIMAL` values stay strings to keep
precision. MySQL errors pass through verbatim so agents can self-correct.

## Next slices (deliberately not in this one)

- Per-user tokens (audit trail: who ran what)
- PII masking — the driver (`go-mysql-org/go-mysql`) already exposes each
  result column's origin table/column (`Field.OrgTable`/`OrgName`), which is
  the foundation; TiDB parser for query gating when needed
- Write support behind a separate, gated path
- Shared VM deployment
