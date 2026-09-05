package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// The two server modes. Read-only is the default and the zero value, so a
// config that never mentions mode — or code that builds a Config directly —
// fails safe.
const (
	modeReadOnly   = "read_only"
	modeFullAccess = "full_access"
)

// The two transports. HTTP is the deployment the project is built around;
// stdio is the single-user path where an MCP client launches the binary as a
// subprocess and speaks JSON-RPC over its pipes.
const (
	transportHTTP  = "http"
	transportStdio = "stdio"
)

type Config struct {
	// Mode gates what the server itself lets through: read_only keeps the
	// session-level write block on every connection, full_access drops it so
	// the MySQL user's grants become the only boundary.
	Mode   string `yaml:"mode"`
	Server struct {
		Listen    string `yaml:"listen"`
		AuthToken string `yaml:"auth_token"`
	} `yaml:"server"`
	Database struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
		DBName   string `yaml:"dbname"`
		// TLS is opt-in per deployment: an absent section means plaintext,
		// which is what a database on the same host or VPC wants.
		TLS *DatabaseTLS `yaml:"tls"`
	} `yaml:"database"`
	Limits struct {
		TimeoutSeconds   int `yaml:"timeout_seconds"`
		MaxResponseBytes int `yaml:"max_response_bytes"`
		MaxConnections   int `yaml:"max_connections"`
	} `yaml:"limits"`
	Logging struct {
		Output   string `yaml:"output"`
		File     string `yaml:"file"`
		Level    string `yaml:"level"`
		Rotation struct {
			MaxSizeMB  int  `yaml:"max_size_mb"`
			MaxBackups int  `yaml:"max_backups"`
			MaxAgeDays int  `yaml:"max_age_days"`
			Compress   bool `yaml:"compress"`
		} `yaml:"rotation"`
	} `yaml:"logging"`
	// Masking is opt-in per deployment: an absent section means off.
	Masking *MaskingConfig `yaml:"masking"`

	// transport is chosen on the command line, not in the file: the client
	// launching a stdio server decides that, and one config can then serve
	// both. Under stdio the Server section is unused and left unexpanded.
	transport string
	// masker is derived from Masking at load time; nil when masking is off.
	masker *Masker
	// tlsConfig is derived from Database.TLS at load time; nil means
	// plaintext. The files it names are read here, so a bad path is a
	// startup error naming the key rather than a failed dial later.
	tlsConfig *tls.Config
	// logLevel is derived from Logging.Level at load time.
	logLevel slog.Level
}

type MaskingConfig struct {
	// Enabled is a pointer so "omitted" is distinguishable from "false":
	// omitted with rules present means true.
	Enabled *bool    `yaml:"enabled"`
	Mask    []string `yaml:"mask"`
	Except  []string `yaml:"except"`
	// Values names the shape detectors (email, phone_id) that scan string
	// cells the column rules left alone. Omitted means no value scanning.
	Values []string `yaml:"values"`
}

// DatabaseTLS encrypts the hop from this server to the database. It only
// asks: the database has to offer TLS, and the driver has no "try TLS, then
// plaintext" mode, so a database without it is a refused connection, never a
// silent downgrade.
type DatabaseTLS struct {
	// Enabled is a pointer so "omitted" is distinguishable from "false":
	// omitted with any other key present means true.
	Enabled *bool `yaml:"enabled"`
	// CA is a PEM file with the certificate(s) that signed the database's.
	// Empty means the system trust store, which covers databases behind a
	// public CA.
	CA string `yaml:"ca"`
	// ServerName is the name to verify the certificate against when it isn't
	// database.host — a certificate issued for db.internal reached over an
	// IP, say. Defaults to database.host.
	ServerName string `yaml:"server_name"`
	// Cert and Key are the PEM client certificate and private key for mutual
	// TLS, for a database user granted with REQUIRE X509. Both or neither.
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	// InsecureSkipVerify keeps the encryption and drops the identity check.
	// A spelled-out choice, because a certificate without a usable name (the
	// one MySQL generates for itself, for instance) can't be verified at all.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// on reports whether the section asks for TLS at all.
func (t *DatabaseTLS) on() bool {
	if t == nil {
		return false
	}
	if t.Enabled != nil {
		return *t.Enabled
	}
	return true
}

// newTLSConfig builds the driver's TLS config from the section, reading the
// files it names. host is the default name to verify against.
func newTLSConfig(t *DatabaseTLS, host string) (*tls.Config, error) {
	if !t.on() {
		return nil, nil
	}
	if t.Enabled == nil && t.CA == "" && t.ServerName == "" && t.Cert == "" && t.Key == "" && !t.InsecureSkipVerify {
		// "tls: {}" says TLS while choosing nothing about it — a
		// misconfiguration, not a choice, same as an empty masking section.
		return nil, fmt.Errorf("database.tls has no settings: write enabled: true to verify against the system trust store, or drop the section")
	}
	if (t.Cert == "") != (t.Key == "") {
		return nil, fmt.Errorf("database.tls.cert and database.tls.key go together (mutual TLS needs both)")
	}
	if t.InsecureSkipVerify && (t.CA != "" || t.ServerName != "") {
		// Go ignores both under InsecureSkipVerify; a config that names them
		// anyway would promise a check that never runs.
		return nil, fmt.Errorf("database.tls.insecure_skip_verify disables the certificate check, so database.tls.ca and database.tls.server_name have no effect — drop them or drop it")
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         host,
		InsecureSkipVerify: t.InsecureSkipVerify,
	}
	if t.ServerName != "" {
		cfg.ServerName = t.ServerName
	}
	if t.CA != "" {
		pem, err := os.ReadFile(t.CA)
		if err != nil {
			return nil, fmt.Errorf("database.tls.ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("database.tls.ca: no certificates found in %s (expected PEM)", t.CA)
		}
		cfg.RootCAs = pool
	}
	if t.Cert != "" {
		pair, err := tls.LoadX509KeyPair(t.Cert, t.Key)
		if err != nil {
			return nil, fmt.Errorf("database.tls.cert / database.tls.key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// LoadConfig reads the YAML file, then expands ${VAR} placeholders from the
// environment. Expansion happens after parsing so secret values containing
// YAML-significant characters (#, :, quotes) can't corrupt the document.
// transport is transportHTTP or transportStdio; under stdio the server
// section is skipped entirely, placeholders included, so a file written for
// the HTTP deployment loads without its token variable set.
func LoadConfig(path, transport string) (*Config, error) {
	switch transport {
	case transportHTTP, transportStdio:
	default:
		return nil, fmt.Errorf("transport must be %q or %q, got %q", transportHTTP, transportStdio, transport)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	cfg.transport = transport
	cfg.Mode = modeReadOnly
	cfg.Server.Listen = "127.0.0.1:3000"
	cfg.Database.Port = 3306
	cfg.Limits.TimeoutSeconds = 30
	cfg.Limits.MaxResponseBytes = 500 << 10
	cfg.Limits.MaxConnections = 10
	cfg.Logging.Output = "stdout"
	cfg.Logging.Level = "info"
	cfg.Logging.Rotation.MaxSizeMB = 100

	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var missing []string
	expand := []*string{
		&cfg.Database.Host, &cfg.Database.Username,
		&cfg.Database.Password, &cfg.Database.DBName,
		&cfg.Logging.File,
	}
	if t := cfg.Database.TLS; t != nil {
		expand = append(expand, &t.CA, &t.ServerName, &t.Cert, &t.Key)
	}
	if !cfg.stdio() {
		expand = append(expand, &cfg.Server.Listen, &cfg.Server.AuthToken)
	}
	for _, f := range expand {
		*f = os.Expand(*f, func(key string) string {
			val, ok := os.LookupEnv(key)
			if !ok {
				missing = append(missing, key)
			}
			return val
		})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %v", missing)
	}

	switch cfg.Mode {
	case modeReadOnly, modeFullAccess:
	default:
		return nil, fmt.Errorf("mode must be %q or %q, got %q", modeReadOnly, modeFullAccess, cfg.Mode)
	}
	if !cfg.stdio() && cfg.Server.AuthToken == "" {
		return nil, fmt.Errorf("server.auth_token must not be empty")
	}
	if cfg.Database.Host == "" || cfg.Database.Username == "" || cfg.Database.DBName == "" {
		return nil, fmt.Errorf("database.host, database.username and database.dbname are required")
	}
	if cfg.Limits.TimeoutSeconds < 1 || cfg.Limits.MaxConnections < 1 || cfg.Limits.MaxResponseBytes < 1 {
		return nil, fmt.Errorf("limits must all be at least 1 (timeout_seconds=%d, max_connections=%d, max_response_bytes=%d)",
			cfg.Limits.TimeoutSeconds, cfg.Limits.MaxConnections, cfg.Limits.MaxResponseBytes)
	}
	switch cfg.Logging.Output {
	case "stdout":
	case "file":
		if cfg.Logging.File == "" {
			return nil, fmt.Errorf(`logging.file is required when logging.output is "file"`)
		}
	default:
		return nil, fmt.Errorf(`logging.output must be "stdout" or "file", got %q`, cfg.Logging.Output)
	}
	if err := cfg.logLevel.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
		return nil, fmt.Errorf("logging.level must be debug, info, warn or error, got %q", cfg.Logging.Level)
	}
	if r := cfg.Logging.Rotation; r.MaxSizeMB < 1 || r.MaxBackups < 0 || r.MaxAgeDays < 0 {
		return nil, fmt.Errorf("logging.rotation needs max_size_mb of at least 1 and no negative values (max_size_mb=%d, max_backups=%d, max_age_days=%d)",
			r.MaxSizeMB, r.MaxBackups, r.MaxAgeDays)
	}
	if cfg.tlsConfig, err = newTLSConfig(cfg.Database.TLS, cfg.Database.Host); err != nil {
		return nil, err
	}
	if cfg.masker, err = NewMasker(cfg.Masking); err != nil {
		return nil, err
	}
	// Under full_access masking is a seatbelt rather than a guarantee, so the
	// masker relaxes for statements it cannot check. That follows from the
	// mode alone — there is nothing for a config key to decide, and making the
	// server refuse to start here only cost a paste of an acknowledgment.
	// main logs the trade-off at startup instead.
	if cfg.masker != nil && cfg.fullAccess() {
		cfg.masker.bestEffort = true
	}
	return cfg, nil
}

func (c *Config) fullAccess() bool { return c.Mode == modeFullAccess }

func (c *Config) stdio() bool { return c.transport == transportStdio }

// tlsOn reports whether connections to the database are encrypted.
func (c *Config) tlsOn() bool { return c.tlsConfig != nil }

// tlsUnverified reports the encrypt-only setting: TLS on, identity check off.
func (c *Config) tlsUnverified() bool { return c.tlsConfig != nil && c.tlsConfig.InsecureSkipVerify }

// console is where output meant for a terminal or a log collector goes:
// stdout normally, stderr under stdio, where stdout is the wire the MCP
// client reads and a single log line on it would corrupt the JSON-RPC
// stream.
func (c *Config) console() io.Writer {
	if c.stdio() {
		return os.Stderr
	}
	return os.Stdout
}

// listensBeyondLoopback reports whether server.listen would accept
// connections from off the host: an empty host (":3000"), a wildcard or
// non-loopback IP, or any name other than localhost. Names are not resolved
// here — a DNS lookup at startup is not worth it, and an unknown name is
// treated as reaching beyond the host. A malformed address is not this
// function's problem: the listen call fails with its own error.
func listensBeyondLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
