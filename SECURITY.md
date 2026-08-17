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
- **Treat `mode: full_access` as what it says.** It removes the server's own
  write block, leaving the MySQL user's grants as the *only* boundary — and an
  agent's writes run with no approval step. Use it only for disposable
  environments (staging) whose data you can restore, with a user scoped to
  exactly that schema, and never for production. If you run PII masking there,
  know its ceiling: it keeps personal data out of an agent's context by
  accident, but a caller with write access can copy data past the rules —
  which is why the config makes you acknowledge that with `best_effort: true`.
  (Masking is best-effort everywhere, in truth: even read-only, values can be
  probed through `WHERE` conditions. Sanitize the data itself when you need a
  guarantee.)
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
