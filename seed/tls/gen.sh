#!/bin/sh
# Regenerates the throwaway PKI the dev database serves TLS with. These files
# are test fixtures, checked in on purpose: the private keys guard nothing but
# a container full of seed rows. Never reuse them anywhere real.
#
# The server certificate carries SANs for every name the tests reach the
# container by (localhost, 127.0.0.1, ::1) plus `db`, which nothing resolves,
# so a test can prove database.tls.server_name does its job. MySQL's own
# auto-generated certificate has no SANs at all, which is why Go can't verify
# it and why these exist.
#
#   sh seed/tls/gen.sh        # then: docker compose down -v && up -d --wait
set -eu
cd "$(dirname "$0")"
days=3650

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout ca-key.pem -out ca.pem -days $days -subj "/CN=mysql-mcp-server test CA" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" 2>/dev/null

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout server-key.pem -out server.csr -subj "/CN=mysql-mcp-server test DB" 2>/dev/null
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out server-cert.pem -days $days \
  -extfile /dev/stdin <<'EXT' 2>/dev/null
subjectAltName=DNS:localhost,DNS:db,IP:127.0.0.1,IP:::1
extendedKeyUsage=serverAuth
EXT

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout client-key.pem -out client.csr -subj "/CN=mcp_x509" 2>/dev/null
openssl x509 -req -in client.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out client-cert.pem -days $days \
  -extfile /dev/stdin <<'EXT' 2>/dev/null
extendedKeyUsage=clientAuth
EXT

rm -f server.csr client.csr ca.srl
# The database image reads the server key as its own unprivileged user.
chmod 644 *.pem
ls -1 *.pem
