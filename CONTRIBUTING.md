# Contributing

Thanks for taking the time to contribute. This is a small, single-package Go
project, so the workflow is short.

## Prerequisites

- Go 1.26 or newer
- Docker or Podman for the throwaway database the integration tests use
  (or a local MySQL/MariaDB you seed yourself — see below)

## Set Up

Clone and build:

```bash
git clone https://github.com/mshddev/mysql-mcp-server.git
cd mysql-mcp-server
go build -o mysql-mcp-server .
```

Start the throwaway dev database. `compose.yaml` runs MariaDB with
`seed/seed.sql` applied on first start:

```bash
docker compose up -d --wait      # or: podman compose up -d --wait
```

Podman reads the same file but needs `podman-compose` on `PATH`
(`brew install podman-compose`, `dnf install podman-compose`, …). Without a
compose provider, the plain `run` form is equivalent:

```bash
podman run -d --name mysql-mcp-dev -p 3306:3306 \
  -e MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1 \
  -v "$PWD/seed/seed.sql:/docker-entrypoint-initdb.d/seed.sql:ro,z" \
  mariadb:10.11
```

If 3306 is already in use, pick another host port with `MYSQL_DEV_PORT=3307`
(or `-p 3307:3306`) and point `MYSQL_TEST_ADDR` at it below.

Verify:

```bash
MYSQL_TEST_ADDR=127.0.0.1:3306 MYSQL_TEST_TLS_CA=seed/tls/ca.pem go test ./...
```

All tests should pass against the seeded `mcp_dev` database. Without
`MYSQL_TEST_ADDR` the integration tests skip and only the unit tests run, so
check with `go test -v . | grep SKIP` if you're unsure whether they ran.

The compose database serves TLS with the certificates in `seed/tls` — a
throwaway CA, a server certificate with SANs for `localhost`, the loopback
addresses and `db`, and a client certificate for the seeded `mcp_x509` user.
`MYSQL_TEST_TLS_CA` turns the TLS tests on; they skip without it, so a
database you seeded yourself needs no certificates. The private keys are
checked in on purpose (they guard a container of fake rows) and
`seed/tls/gen.sh` regenerates the set; after running it, recreate the
container so the server picks up the new files.

The seed only runs when the data directory is empty. To start over — after
a write test leaves something behind, or after editing the seed — recreate the
volume:

```bash
docker compose down -v && docker compose up -d --wait
```

or re-apply the seed in place, which drops and rebuilds `mcp_dev`:

```bash
docker compose exec -T db mariadb -u root < seed/seed.sql
```

Set `MYSQL_DEV_IMAGE=mysql:8.4` to run the suite against MySQL instead of
MariaDB; the two differ in a few places (see the comment on the view in
`seed/seed.sql`) and the tests pin whichever the server exhibits. CI runs
both. To have both engines up side by side, give the second one its own port
and compose project name so it doesn't replace the first:

```bash
MYSQL_DEV_PORT=3308 MYSQL_DEV_IMAGE=mysql:8.4 docker compose -p mcp84 up -d --wait
MYSQL_TEST_ADDR=127.0.0.1:3308 go test ./...
docker compose -p mcp84 down -v
```

**Without containers.** Any local MySQL or MariaDB works too. Seed it and
point the tests at it:

```bash
mysql -h 127.0.0.1 -u root < seed/seed.sql
MYSQL_TEST_ADDR=127.0.0.1:3306 go test ./...
```

Override the credentials with `MYSQL_TEST_USER`, `MYSQL_TEST_PASSWORD` and
`MYSQL_TEST_DATABASE` if yours differ from the seed (`MYSQL_TEST_WRITE_USER`
and `MYSQL_TEST_WRITE_PASSWORD` for the `full_access` tests, and
`MYSQL_TEST_X509_USER` / `MYSQL_TEST_X509_PASSWORD` for the mutual-TLS ones;
both sets skip when their user is missing).

## Before You Open a PR

Format, vet, and test:

```bash
gofmt -w .
go vet ./...
MYSQL_TEST_ADDR=127.0.0.1:3306 MYSQL_TEST_TLS_CA=seed/tls/ca.pem go test ./...
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
