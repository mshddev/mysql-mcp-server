# About This Project
MCP server for MySQL/MariaDB: runs raw SQL against MySQL/MariaDB then give the results back in json format.
Remote-first: one instance runs near the database over streamable HTTP with a bearer token, and a team's agents connect to its URL. `--stdio` is the secondary, single-user path (the client launches the binary; no token; logs on stderr), and the docs should present it as that, never as the main deployment.

# Commands

```sh
go build -o mysql-mcp-server .        # build
go vet ./...                          # static checks (no other lint configured)
go test ./...                         # unit tests; integration tests self-skip
go test -run TestName .               # single test
MYSQL_TEST_ADDR=127.0.0.1:3306 go test ./...   # also run integration tests
MYSQL_TEST_ADDR=127.0.0.1:3306 MYSQL_TEST_TLS_CA=seed/tls/ca.pem go test ./...   # + the TLS ones (compose DB serves seed/tls)
docker compose up -d --wait                    # seeded dev DB in a container (podman compose works too)
mysql -h 127.0.0.1 -u root < seed/seed.sql     # or seed a local MySQL/MariaDB yourself
MYSQL_MCP_AUTH_TOKEN=... MYSQL_PASSWORD=... ./mysql-mcp-server --config ./config.yaml
MYSQL_PASSWORD=... ./mysql-mcp-server --stdio --config ./config.yaml   # single-user, client launches it
```

Integration tests need a database seeded with `seed/seed.sql`; connection
defaults (`mcp_readonly` / `devpassword` / `mcp_dev`) can be overridden with
`MYSQL_TEST_USER`, `MYSQL_TEST_PASSWORD`, `MYSQL_TEST_DATABASE`. The
full_access write tests use the seeded `mcp_write` / `devpassword` user
(override with `MYSQL_TEST_WRITE_USER` / `MYSQL_TEST_WRITE_PASSWORD`) and
self-skip when it's missing. The TLS tests need `MYSQL_TEST_TLS_CA`
(the compose database serves the throwaway certs in `seed/tls`; the mutual-TLS
tests use the seeded `mcp_x509` user) and self-skip without it. Local dev is
often MariaDB, which the tests and code both accommodate.
