# Contributing

Thanks for taking the time to contribute. This is a small, single-package Go
project, so the workflow is short.

## Prerequisites

- Go 1.26 or newer
- A local MySQL or MariaDB (the integration tests need one)

## Set Up

Clone and build:

```bash
git clone https://github.com/mshddev/mysql-mcp-server.git
cd mysql-mcp-server
go build -o mysql-mcp-server .
```

Seed the throwaway dev database:

```bash
mysql -h 127.0.0.1 -u root < seed/seed.sql
```

verify:

```bash
MYSQL_TEST_ADDR=127.0.0.1:3306 go test ./...
```

All tests should pass against the seeded `mcp_dev` database. Without
`MYSQL_TEST_ADDR` the integration tests skip and only the unit tests run.
Override the credentials with `MYSQL_TEST_USER`, `MYSQL_TEST_PASSWORD` and
`MYSQL_TEST_DATABASE` if yours differ from the seed.

## Before You Open a PR

Format, vet, and test:

```bash
gofmt -w .
go vet ./...
MYSQL_TEST_ADDR=127.0.0.1:3306 go test ./...
```

Keep changes focused and match the surrounding style. Add tests for new
behavior. Anything that touches the safety model — read-only enforcement, the
caps, auth — should ship with a test that proves the guard holds, not just that
the happy path works.

## Commits and PRs

- Write imperative commit subjects (`Add …`, `Fix …`, `Rename …`).
- One logical change per PR where you can.
- Say what changed and why. If it changes behavior, call that out.

## Reporting Bugs and Ideas

Open an issue with enough to reproduce — the SQL, the config (secrets redacted),
and what you expected. For security problems, do not open a public issue; see
[SECURITY.md](SECURITY.md).

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
