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

This server hands an AI agent live SQL access, so the deployment matters as much
as the code. A few things to get right:

- **Use a `SELECT`-only user, scoped to one database.** Read-only is enforced by
  the database's grants first, and by `SET SESSION TRANSACTION READ ONLY` second.
  A user with wider grants widens the blast radius — point the server at a
  read-only replica where you can.
- **Bind to loopback unless you mean to expose it.** `listen: 127.0.0.1:3000` by
  default; only widen it behind something you trust.
- **Use a strong, unique bearer token,** and rotate it if it leaks. It's the
  only thing between a caller and the data.
- **Keep secrets in the environment,** not in `config.yaml`. The `${VAR}`
  placeholders exist for this — the token and password should never sit in a
  file in git.
- **Remember the response cap bounds the JSON, not the database read.** A query
  can still pull a large payload from MySQL before truncation; the timeout is the
  backstop for that.
