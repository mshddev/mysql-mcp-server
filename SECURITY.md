# Security Policy

## Supported Versions

This project is pre-1.0. Only the latest release on the `main` branch receives
security fixes.

## Reporting a Vulnerability

Please report vulnerabilities privately — **do not open a public issue.**

Use GitHub's private reporting: go to the **Security** tab →
**Report a vulnerability**. Include steps to reproduce, the impact you see, and
a suggested fix if you have one. You'll get an acknowledgement, and a fix or a
decision before any public disclosure.

## Running It Safely

This server hands AI agents live SQL access, and it is meant to run as one
shared service on the network, so the deployment matters as much as the code.
A few things to get right:

- **Use a `SELECT`-only user, scoped to one database.** Read-only is enforced by
  the database's grants first, and by `SET SESSION TRANSACTION READ ONLY` second.
  A user with wider grants widens the blast radius — point the server at a
  read-only replica where you can.
- **Treat `mode: full_access` as what it says.** It removes the server's own
  write block, leaving the MySQL user's grants as the *only* boundary — and an
  agent's writes run with no approval step. Use it only for disposable
  environments (staging) whose data you can restore, with a user scoped to
  exactly that schema, and never for production. If you run PII masking there,
  know its ceiling: it keeps personal data out of an agent's context by
  accident, but a caller with write access can copy data past the rules — only
  `masking.values` still sees such a copy, and only for values with a
  recognisable shape. The server warns about that at startup rather than
  blocking it, so read your logs on the first boot of a full_access deployment.
  (Masking is best-effort everywhere, in truth: even read-only, values can be
  probed through `WHERE` conditions. Sanitize the data itself when you need a
  guarantee.)
- **The binary speaks plain HTTP, so terminate TLS in front of it.** It binds
  `127.0.0.1:3000` by default on purpose: the expected shape is a reverse proxy
  on the same host doing TLS, with only the proxy's port open to the network
  (the README's Deploy section walks through it). Widen `listen` only on a
  private network you trust, knowing the tokens then cross it in the clear.
  The only paths that answer without a token are the health probes,
  `/healthz` and `/readyz`, and they say up or down and nothing else; the
  readiness one caches its database ping for five seconds so an anonymous
  caller can't turn it into load.
- **The hop to the database is plaintext unless `database.tls` says
  otherwise.** That is the right default for the shape above, where the
  database is on the same host or private network. When it isn't — a managed
  database, or a `--stdio` server on a laptop reaching one elsewhere — turn
  `database.tls` on; it verifies the certificate by default and warns at
  startup if you opt out of that (README, Encrypting the database hop).
- **Issue one strong bearer token per person or agent,** and revoke or
  rotate that one entry if it leaks. A token is the only thing between a
  caller and the data, and its `server.auth_tokens` name is what the query
  log records as `caller`: share one between people and the log can no
  longer say who ran what. The server refuses a config where two names carry
  the same token.
- **Keep secrets in the environment,** not in `config.yaml`. The `${VAR}`
  placeholders exist for this — the tokens and password should never sit in a
  file in git.
- **Remember the response cap bounds the JSON, not the database read.** A query
  can still pull a large payload from MySQL before truncation; the timeout is the
  backstop for that.
