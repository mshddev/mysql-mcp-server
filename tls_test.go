package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Unit test: no database needed.
// ---------------------------------------------------------------------------

// A certificate whose only name is its Common Name — the shape MySQL and
// MariaDB generate for themselves at first start — can't be verified by Go at
// all, whatever database.tls.server_name says. The README claims as much;
// this pins it, along with the advice the connect error gives for it.
func TestTLSCertificateWithoutSANIsUnverifiable(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "MySQL_Server_8.4.11_Auto_Generated_Server_Certificate"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true, // self-signed, like MySQL's
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = c.Read(make([]byte, 1)); c.Close() }()
		}
	}()

	// Trust the certificate's own CA and name it as the server — the most
	// generous config possible. Go still refuses: no SAN, no verification.
	pool := x509.NewCertPool()
	pool.AddCert(tmpl)
	cfg := &tls.Config{RootCAs: pool, ServerName: tmpl.Subject.CommonName, MinVersion: tls.VersionTLS12}
	conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded against a certificate with no SAN, want a hostname error")
	}
	var hostErr x509.HostnameError
	if !errors.As(err, &hostErr) {
		t.Fatalf("error %T %q, want x509.HostnameError", err, err)
	}
	if !strings.Contains(err.Error(), "Common Name") {
		t.Errorf("error %q should say the certificate relies on the legacy Common Name", err)
	}
	if got := describeConnectError(err); !strings.Contains(got, "insecure_skip_verify") {
		t.Errorf("describeConnectError = %q, want it to offer insecure_skip_verify as the way out", got)
	}
}

// ---------------------------------------------------------------------------
// Integration tests: skipped unless the database serves TLS with a known CA.
// ---------------------------------------------------------------------------

// Set MYSQL_TEST_TLS_CA to the PEM file that signed the database's
// certificate (seed/tls/ca.pem for the compose database, which serves the
// certificates in that directory). The mutual-TLS tests also look for
// client-cert.pem and client-key.pem beside it, and for the seeded mcp_x509
// user (override with MYSQL_TEST_X509_USER / MYSQL_TEST_X509_PASSWORD).
const tlsCAEnv = "MYSQL_TEST_TLS_CA"

func tlsCA(t *testing.T) string {
	t.Helper()
	ca := os.Getenv(tlsCAEnv)
	if ca == "" {
		t.Skipf("set %s (e.g. seed/tls/ca.pem) to run TLS tests", tlsCAEnv)
	}
	return ca
}

// newTLSPool is newTestPool with a database.tls section applied the way
// LoadConfig would apply it, including the derived tls.Config.
func newTLSPool(t *testing.T, section *DatabaseTLS, adjust func(*Config)) *Pool {
	t.Helper()
	return newTestPool(t, func(cfg *Config) {
		if adjust != nil {
			adjust(cfg)
		}
		cfg.Database.TLS = section
		var err error
		if cfg.tlsConfig, err = newTLSConfig(section, cfg.Database.Host); err != nil {
			t.Fatalf("newTLSConfig: %v", err)
		}
	})
}

// sslCipher reports the cipher of the pooled connection, or "" in plaintext.
func sslCipher(t *testing.T, p *Pool) string {
	t.Helper()
	res := mustQuery(t, p, "SHOW SESSION STATUS LIKE 'Ssl_cipher'")
	if len(res.Rows) != 1 {
		t.Fatalf("Ssl_cipher returned %d rows, want 1", len(res.Rows))
	}
	v, _ := res.Rows[0]["Value"].(string)
	return v
}

// connectError runs a query expected to fail at connection time and returns
// its describeConnectError text.
func connectError(t *testing.T, p *Pool) string {
	t.Helper()
	res, err := query(t, p, "SELECT 1")
	if err == nil {
		t.Fatalf("query succeeded (result %+v), want a connection error", res)
	}
	return describeConnectError(err)
}

func TestTLSPlaintextByDefault(t *testing.T) {
	tlsCA(t)
	p := newTestPool(t, nil)
	if c := sslCipher(t, p); c != "" {
		t.Errorf("Ssl_cipher = %q with no tls section, want plaintext", c)
	}
}

func TestTLSVerifiedConnect(t *testing.T) {
	p := newTLSPool(t, &DatabaseTLS{CA: tlsCA(t)}, nil)
	if c := sslCipher(t, p); c == "" {
		t.Fatal("Ssl_cipher empty, want an encrypted session")
	}
	res := mustQuery(t, p, "SHOW SESSION STATUS LIKE 'Ssl_version'")
	if v, _ := res.Rows[0]["Value"].(string); !strings.HasPrefix(v, "TLSv1.2") && !strings.HasPrefix(v, "TLSv1.3") {
		t.Errorf("Ssl_version = %q, want TLS 1.2 or 1.3", v)
	}
}

func TestTLSUnverifiedConnect(t *testing.T) {
	tlsCA(t)
	p := newTLSPool(t, &DatabaseTLS{InsecureSkipVerify: true}, nil)
	if c := sslCipher(t, p); c == "" {
		t.Fatal("Ssl_cipher empty, want an encrypted session")
	}
}

func TestTLSRejectsUnknownCA(t *testing.T) {
	tlsCA(t)
	other := writeTestCerts(t)
	p := newTLSPool(t, &DatabaseTLS{CA: other.ca}, nil)
	got := connectError(t, p)
	if !strings.HasPrefix(got, "database certificate is signed by a CA this server doesn't trust") {
		t.Errorf("connect error = %q, want the unknown-CA explanation", got)
	}
}

func TestTLSRejectsWrongServerName(t *testing.T) {
	p := newTLSPool(t, &DatabaseTLS{CA: tlsCA(t), ServerName: "not-the-database.invalid"}, nil)
	got := connectError(t, p)
	if !strings.HasPrefix(got, "database certificate is not for this host") {
		t.Errorf("connect error = %q, want the hostname explanation", got)
	}
}

// The compose certificate carries `db` as a SAN that nothing resolves, so
// reaching the database by address while verifying it as `db` proves the
// override is what gets checked.
func TestTLSServerNameOverride(t *testing.T) {
	p := newTLSPool(t, &DatabaseTLS{CA: tlsCA(t), ServerName: "db"}, nil)
	if c := sslCipher(t, p); c == "" {
		t.Fatal("Ssl_cipher empty, want an encrypted session")
	}
}

// x509User points a config at the seeded REQUIRE X509 user, with or without
// the client certificate beside the CA.
func x509User(t *testing.T, withCert bool) (*DatabaseTLS, func(*Config)) {
	t.Helper()
	ca := tlsCA(t)
	section := &DatabaseTLS{CA: ca}
	if withCert {
		section.Cert = filepath.Join(filepath.Dir(ca), "client-cert.pem")
		section.Key = filepath.Join(filepath.Dir(ca), "client-key.pem")
		for _, f := range []string{section.Cert, section.Key} {
			if _, err := os.Stat(f); err != nil {
				t.Skipf("client certificate unavailable beside %s: %v", tlsCAEnv, err)
			}
		}
	}
	return section, func(cfg *Config) {
		cfg.Database.Username = envOr("MYSQL_TEST_X509_USER", "mcp_x509")
		cfg.Database.Password = envOr("MYSQL_TEST_X509_PASSWORD", "devpassword")
	}
}

// newX509Pool connects as the REQUIRE X509 user with the client certificate,
// skipping when the user is missing.
func newX509Pool(t *testing.T, adjust func(*Config)) *Pool {
	t.Helper()
	section, user := x509User(t, true)
	p := newTLSPool(t, section, func(cfg *Config) {
		user(cfg)
		if adjust != nil {
			adjust(cfg)
		}
	})
	if _, err := query(t, p, "SELECT 1"); err != nil {
		t.Skipf("x509 user unavailable — re-seed with seed/seed.sql: %v", err)
	}
	return p
}

func TestTLSMutualAuth(t *testing.T) {
	p := newX509Pool(t, nil)
	if c := sslCipher(t, p); c == "" {
		t.Fatal("Ssl_cipher empty, want an encrypted session")
	}
	res := mustQuery(t, p, "SELECT COUNT(*) AS n FROM users")
	if asNumber(t, res.Rows[0]["n"]) < 1 {
		t.Error("x509 user's SELECT grant not in effect")
	}

	// The same user without the certificate is a refused login: the
	// requirement lives in the grant, so the server enforces it, not us.
	section, user := x509User(t, false)
	got := connectError(t, newTLSPool(t, section, user))
	if !strings.HasPrefix(got, "database login refused") {
		t.Errorf("connect error without a client certificate = %q, want a refused login", got)
	}
}

// The kill connection is a separate dial. As the REQUIRE X509 user it can
// only succeed if that dial carries the same TLS config as the pool's, so a
// runaway query that is actually gone from the server proves it does.
func TestTLSKillQueryUsesSameTransport(t *testing.T) {
	p := newX509Pool(t, func(cfg *Config) {
		cfg.Limits.MaxConnections = 1
		cfg.Limits.TimeoutSeconds = 20
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if _, err := p.Query(ctx, "SELECT SUM(a.id * b.id * c.id) FROM big a, big b, big c"); err == nil {
		t.Fatal("runaway query succeeded, want a timeout")
	}
	// A kill that couldn't connect fails quietly and leaves the query to the
	// 20s server-side fence, so the time to return is the tell.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("query took %v, want it killed shortly after the 1s deadline — did killQuery dial without TLS?", elapsed)
	}

	time.Sleep(200 * time.Millisecond)
	res := mustQuery(t, p,
		"SELECT COUNT(*) AS n FROM information_schema.processlist WHERE command = 'Query' AND time > 0")
	if orphans := asNumber(t, res.Rows[0]["n"]); orphans != 0 {
		t.Errorf("%v query(ies) still running after the kill, want 0 — did killQuery dial without TLS?", orphans)
	}
}
