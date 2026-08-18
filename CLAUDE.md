# About This Project
MCP server for MySQL/MariaDB that basically just runs raw SQL against MySQL/MariaDB then give the results back in json format.

# Commands

```sh
go build -o mysql-mcp-server .        # build
go vet ./...                          # static checks (no other lint configured)
go test ./...                         # unit tests; integration tests self-skip
go test -run TestName .               # single test
MYSQL_TEST_ADDR=127.0.0.1:3306 go test ./...   # also run integration tests
mysql -h 127.0.0.1 -u root < seed/seed.sql     # seed the local dev database
MYSQL_MCP_AUTH_TOKEN=... MYSQL_PASSWORD=... ./mysql-mcp-server --config ./config.yaml
```

Integration tests need a database seeded with `seed/seed.sql`; connection
defaults (`mcp_readonly` / `devpassword` / `mcp_dev`) can be overridden with
`MYSQL_TEST_USER`, `MYSQL_TEST_PASSWORD`, `MYSQL_TEST_DATABASE`. The
full_access write tests use the seeded `mcp_write` / `devpassword` user
(override with `MYSQL_TEST_WRITE_USER` / `MYSQL_TEST_WRITE_PASSWORD`) and
self-skip when it's missing. Local dev is
often MariaDB, which the tests and code both accommodate.
