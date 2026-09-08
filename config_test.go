package main

import (
	"crypto/tls"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config that passes every validation rule, used as the starting point for
// the failure cases below.
const validConfig = `
server:
  auth_tokens:
    dev: s3cret
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`

// unsetVar must never exist in the environment; the missing-variable cases
// depend on it staying unset.
const unsetVar = "MYSQL_MCP_TEST_UNSET_VAR"

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, validConfig), transportHTTP)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Server.Listen != "127.0.0.1:3000" {
		t.Errorf("Listen = %q, want 127.0.0.1:3000", cfg.Server.Listen)
	}
	if cfg.Database.Port != 3306 {
		t.Errorf("Port = %d, want 3306", cfg.Database.Port)
	}
	if cfg.Limits.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds = %d, want 30", cfg.Limits.TimeoutSeconds)
	}
	if cfg.Limits.MaxResponseBytes != 500<<10 {
		t.Errorf("MaxResponseBytes = %d, want %d", cfg.Limits.MaxResponseBytes, 500<<10)
	}
	if cfg.Limits.MaxConnections != 10 {
		t.Errorf("MaxConnections = %d, want 10", cfg.Limits.MaxConnections)
	}
	if cfg.Logging.Output != "stdout" {
		t.Errorf("Logging.Output = %q, want stdout", cfg.Logging.Output)
	}
	if cfg.Logging.Level != "info" || cfg.logLevel != slog.LevelInfo {
		t.Errorf("Logging.Level = %q (parsed %v), want info", cfg.Logging.Level, cfg.logLevel)
	}
	if cfg.Logging.Rotation.MaxSizeMB != 100 {
		t.Errorf("Logging.Rotation.MaxSizeMB = %d, want 100", cfg.Logging.Rotation.MaxSizeMB)
	}
}

func TestLoadConfigLogging(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantLevel slog.Level
		wantErr   string
	}{
		{
			name: "file output with rotation",
			body: validConfig + `
logging:
  output: file
  file: /var/log/mcp/server.log
  level: warn
  rotation:
    max_size_mb: 10
    max_backups: 3
    max_age_days: 7
    compress: true
`,
			wantLevel: slog.LevelWarn,
		},
		{
			// Level names are case-insensitive, like slog itself accepts.
			name:      "uppercase level",
			body:      validConfig + "logging:\n  level: DEBUG\n",
			wantLevel: slog.LevelDebug,
		},
		{
			name:    "file output without a path",
			body:    validConfig + "logging:\n  output: file\n",
			wantErr: "logging.file is required",
		},
		{
			name:    "unknown output",
			body:    validConfig + "logging:\n  output: syslog\n",
			wantErr: "logging.output",
		},
		{
			name:    "unknown level",
			body:    validConfig + "logging:\n  level: verbose\n",
			wantErr: "logging.level",
		},
		{
			name:    "zero max_size_mb",
			body:    validConfig + "logging:\n  rotation:\n    max_size_mb: 0\n",
			wantErr: "logging.rotation",
		},
		{
			name:    "negative max_backups",
			body:    validConfig + "logging:\n  rotation:\n    max_backups: -1\n",
			wantErr: "logging.rotation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tt.body), transportHTTP)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConfig succeeded, want an error mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.logLevel != tt.wantLevel {
				t.Errorf("logLevel = %v, want %v", cfg.logLevel, tt.wantLevel)
			}
		})
	}
}

func TestLoadConfigOverridesDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
server:
  listen: 0.0.0.0:9999
  auth_tokens:
    dev: s3cret
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
limits:
  timeout_seconds: 5
  max_response_bytes: 1024
  max_connections: 2
`), transportHTTP)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:9999" {
		t.Errorf("Listen = %q, want 0.0.0.0:9999", cfg.Server.Listen)
	}
	if cfg.Limits.TimeoutSeconds != 5 || cfg.Limits.MaxResponseBytes != 1024 || cfg.Limits.MaxConnections != 2 {
		t.Errorf("limits = %+v, want {5 1024 2}", cfg.Limits)
	}
}

// Expansion runs after the YAML parse, so a secret may contain characters that
// would otherwise change the meaning of the document.
func TestLoadConfigEnvExpansion(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"comment marker", "p#ssword"},
		{"colon", "user:pass"},
		{"double quote", `pa"ss`},
		{"single quote", "pa'ss"},
		{"yaml document start", "---"},
		{"leading and trailing space", "  spaced  "},
		{"newline", "line1\nline2"},
		{"everything", "p#ss:w\"rd'{}[]&*!|>%@`"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MCP_TEST_DB_PASSWORD", tt.value)
			cfg, err := LoadConfig(writeConfig(t, `
server:
  auth_tokens:
    dev: s3cret
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: ${MCP_TEST_DB_PASSWORD}
  dbname: mcp_dev
`), transportHTTP)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.Database.Password != tt.value {
				t.Errorf("Password = %q, want %q", cfg.Database.Password, tt.value)
			}
		})
	}
}

func TestLoadConfigExpandsEveryField(t *testing.T) {
	t.Setenv("MCP_TEST_LISTEN", "0.0.0.0:1234")
	t.Setenv("MCP_TEST_TOKEN", "tok#en")
	t.Setenv("MCP_TEST_HOST", "db.internal")
	t.Setenv("MCP_TEST_USER", "reader")
	t.Setenv("MCP_TEST_PASSWORD", "pw:1")
	t.Setenv("MCP_TEST_DATABASE", "shop")
	t.Setenv("MCP_TEST_LOG_DIR", "/var/log/mcp")

	cfg, err := LoadConfig(writeConfig(t, `
server:
  listen: ${MCP_TEST_LISTEN}
  auth_tokens:
    dev: ${MCP_TEST_TOKEN}
database:
  host: ${MCP_TEST_HOST}
  username: ${MCP_TEST_USER}
  password: ${MCP_TEST_PASSWORD}
  dbname: ${MCP_TEST_DATABASE}
logging:
  output: file
  file: ${MCP_TEST_LOG_DIR}/server.log
`), transportHTTP)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	got := []string{
		cfg.Server.Listen, cfg.Server.AuthTokens["dev"], cfg.Database.Host,
		cfg.Database.Username, cfg.Database.Password, cfg.Database.DBName,
		cfg.Logging.File,
	}
	want := []string{"0.0.0.0:1234", "tok#en", "db.internal", "reader", "pw:1", "shop",
		"/var/log/mcp/server.log"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadConfigMasking(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantMasker bool
		wantErr    string
	}{
		{name: "absent section is off", body: validConfig, wantMasker: false},
		{
			// "masking:" with no value parses as null and leaves the pointer
			// nil — off, same as absent. Distinct from "masking: {}" below.
			name:       "null section is off",
			body:       validConfig + "masking:\n",
			wantMasker: false,
		},
		{
			// An explicitly empty section says "masking" while masking
			// nothing: a misconfiguration, not a choice.
			name:    "empty section is an error",
			body:    validConfig + "masking: {}\n",
			wantErr: "no rules",
		},
		{
			name:       "rules without enabled are active",
			body:       validConfig + "masking:\n  mask: [phone, \"*_phone\", users.address]\n",
			wantMasker: true,
		},
		{
			name:       "enabled false is off",
			body:       validConfig + "masking:\n  enabled: false\n  mask: [phone]\n",
			wantMasker: false,
		},
		{
			name:    "enabled true without rules is an error",
			body:    validConfig + "masking:\n  enabled: true\n",
			wantErr: "no rules",
		},
		{
			name:    "bad glob is an error",
			body:    validConfig + "masking:\n  mask: [\"[bad\"]\n",
			wantErr: "masking.mask",
		},
		{
			name:    "bad glob is an error even while disabled",
			body:    validConfig + "masking:\n  enabled: false\n  mask: [\"[bad\"]\n",
			wantErr: "masking.mask",
		},
		{
			name:       "values alone are active",
			body:       validConfig + "masking:\n  values: [email, phone_id]\n",
			wantMasker: true,
		},
		{
			name:    "unknown detector is an error",
			body:    validConfig + "masking:\n  mask: [phone]\n  values: [credit_card]\n",
			wantErr: "masking.values",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tt.body), transportHTTP)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConfig succeeded, want an error mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if (cfg.masker != nil) != tt.wantMasker {
				t.Errorf("masker = %v, want present: %v", cfg.masker, tt.wantMasker)
			}
		})
	}
}

func TestLoadConfigMode(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantFullAccess bool
		wantBestEffort bool
		wantErr        string
	}{
		{name: "absent mode is read_only", body: validConfig},
		{name: "explicit read_only", body: validConfig + "mode: read_only\n"},
		{name: "full_access", body: validConfig + "mode: full_access\n", wantFullAccess: true},
		{name: "unknown mode is an error", body: validConfig + "mode: read_write\n", wantErr: "mode"},
		{
			// Write access defeats a mask list (a write can copy PII into
			// tables the rules don't name), so masking relaxes here. It
			// follows from the mode and needs no acknowledgment key.
			name:           "full_access with masking starts and is best-effort",
			body:           validConfig + "mode: full_access\nmasking:\n  mask: [phone]\n",
			wantFullAccess: true,
			wantBestEffort: true,
		},
		{
			// Disabled masking leaves no masker to relax.
			name:           "full_access with disabled masking",
			body:           validConfig + "mode: full_access\nmasking:\n  enabled: false\n  mask: [phone]\n",
			wantFullAccess: true,
		},
		{
			// read_only keeps full enforcement: nothing is best-effort there.
			name: "read_only masking is fully enforced",
			body: validConfig + "masking:\n  mask: [phone]\n",
		},
		{
			// The retired acknowledgment key. yaml ignores unknown fields, so
			// an old config keeps working; behaviour now comes from the mode.
			name: "stale best_effort key is ignored",
			body: validConfig + "masking:\n  mask: [phone]\n  best_effort: true\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tt.body), transportHTTP)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConfig succeeded, want an error mentioning %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.fullAccess() != tt.wantFullAccess {
				t.Errorf("fullAccess() = %v, want %v", cfg.fullAccess(), tt.wantFullAccess)
			}
			if gotBE := cfg.masker != nil && cfg.masker.bestEffort; gotBE != tt.wantBestEffort {
				t.Errorf("masker best-effort = %v, want %v", gotBE, tt.wantBestEffort)
			}
		})
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*testing.T)
		body    string
		wantIn  []string
		wantOut string
	}{
		{
			name: "missing auth tokens",
			body: `
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"server.auth_tokens", "at least one"},
		},
		{
			name:   "empty auth tokens map",
			body:   strings.Replace(validConfig, "auth_tokens:\n    dev: s3cret", "auth_tokens: {}", 1),
			wantIn: []string{"server.auth_tokens", "at least one"},
		},
		{
			name:  "auth token expands to empty",
			setup: func(t *testing.T) { t.Setenv("MCP_TEST_TOKEN", "") },
			body: `
server:
  auth_tokens:
    dev: ${MCP_TEST_TOKEN}
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"server.auth_tokens.dev", "empty"},
		},
		{
			name:   "token with whitespace",
			body:   strings.Replace(validConfig, "dev: s3cret", `dev: "s3c ret"`, 1),
			wantIn: []string{"server.auth_tokens.dev", "whitespace"},
		},
		{
			// strings.Fields splits the header on Unicode whitespace, so a
			// non-breaking space pasted into a token would 401 forever.
			name:   "token with a non-breaking space",
			body:   strings.Replace(validConfig, "dev: s3cret", `dev: "s3c\u00a0ret"`, 1),
			wantIn: []string{"server.auth_tokens.dev", "whitespace"},
		},
		{
			name:   "retired auth_token key names the replacement",
			body:   strings.Replace(validConfig, "auth_tokens:\n    dev: s3cret", "auth_token: s3cret", 1),
			wantIn: []string{"server.auth_token was replaced by server.auth_tokens"},
		},
		{
			name:   "caller name with a space",
			body:   strings.Replace(validConfig, "dev: s3cret", `"a b": s3cret`, 1),
			wantIn: []string{"server.auth_tokens", `"a b"`},
		},
		{
			name:   "caller name too long",
			body:   strings.Replace(validConfig, "dev: s3cret", strings.Repeat("x", 65)+": s3cret", 1),
			wantIn: []string{"server.auth_tokens", "64"},
		},
		{
			// Two names on one token would leave the log guessing which
			// of them ran a query.
			name:   "two callers share a token",
			body:   strings.Replace(validConfig, "dev: s3cret", "bob: s3cret\n    alice: s3cret", 1),
			wantIn: []string{"server.auth_tokens", "alice and bob share one token"},
		},
		{
			name: "missing host",
			body: `
server:
  auth_tokens:
    dev: s3cret
database:
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"database.host"},
		},
		{
			name: "missing username",
			body: `
server:
  auth_tokens:
    dev: s3cret
database:
  host: 127.0.0.1
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"database.username"},
		},
		{
			name: "missing dbname",
			body: `
server:
  auth_tokens:
    dev: s3cret
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
`,
			wantIn: []string{"database.dbname"},
		},
		{
			name:   "zero timeout",
			body:   validConfig + "limits:\n  timeout_seconds: 0\n",
			wantIn: []string{"timeout_seconds=0"},
		},
		{
			name:   "negative timeout",
			body:   validConfig + "limits:\n  timeout_seconds: -1\n",
			wantIn: []string{"timeout_seconds=-1"},
		},
		{
			name:   "zero max connections",
			body:   validConfig + "limits:\n  max_connections: 0\n",
			wantIn: []string{"max_connections=0"},
		},
		{
			name:   "negative max connections",
			body:   validConfig + "limits:\n  max_connections: -4\n",
			wantIn: []string{"max_connections=-4"},
		},
		{
			name:   "zero max response bytes",
			body:   validConfig + "limits:\n  max_response_bytes: 0\n",
			wantIn: []string{"max_response_bytes=0"},
		},
		{
			name:   "negative max response bytes",
			body:   validConfig + "limits:\n  max_response_bytes: -1\n",
			wantIn: []string{"max_response_bytes=-1"},
		},
		{
			name: "unset environment variable",
			body: `
server:
  auth_tokens:
    dev: ${` + unsetVar + `}
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"unset environment variables", unsetVar},
			// The name of the variable is reported, never a partly-expanded value.
			wantOut: "must not be empty",
		},
		{
			name: "several unset environment variables",
			body: `
server:
  auth_tokens:
    dev: ${` + unsetVar + `_A}
database:
  host: ${` + unsetVar + `_B}
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{unsetVar + "_A", unsetVar + "_B"},
		},
		{
			name:   "malformed yaml",
			body:   "server:\n  auth_tokens: [unclosed\n",
			wantIn: []string{"parse config"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			cfg, err := LoadConfig(writeConfig(t, tt.body), transportHTTP)
			if err == nil {
				t.Fatalf("LoadConfig succeeded, want error (got %+v)", cfg)
			}
			if cfg != nil {
				t.Errorf("config = %+v, want nil alongside the error", cfg)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if tt.wantOut != "" && strings.Contains(err.Error(), tt.wantOut) {
				t.Errorf("error %q should not mention %q", err, tt.wantOut)
			}
		})
	}
}

func TestLoadConfigUnreadableFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"), transportHTTP)
	if err == nil {
		t.Fatal("LoadConfig succeeded on a missing file, want error")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error = %q, want it to mention %q", err, "read config")
	}
}

func TestUnsetVarIsUnset(t *testing.T) {
	for _, name := range []string{unsetVar, unsetVar + "_A", unsetVar + "_B"} {
		if v, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s is set to %q in the environment; the missing-variable tests need it unset", name, v)
		}
	}
}

// Under stdio the client that launched the process is the only caller, so
// there is no token to check and the server section is not even read: a
// file written for the HTTP deployment, placeholder token and all, must load
// without that variable in the environment.
func TestLoadConfigStdio(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "no server section",
			body: `
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
		},
		{
			name: "unset token placeholder is left alone",
			body: `
server:
  listen: ${` + unsetVar + `}
  auth_tokens:
    dev: ${` + unsetVar + `}
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tt.body), transportStdio)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if !cfg.stdio() {
				t.Error("stdio() = false, want true")
			}
			if cfg.console() != os.Stderr {
				t.Error("console() is not stderr; a log line on stdout would corrupt the MCP stream")
			}
		})
	}

	// The database section is still validated: stdio changes who may call,
	// not what the server needs to answer.
	_, err := LoadConfig(writeConfig(t, `
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: ${`+unsetVar+`}
  dbname: mcp_dev
`), transportStdio)
	if err == nil || !strings.Contains(err.Error(), unsetVar) {
		t.Errorf("unset database placeholder under stdio: err = %v, want it to name %s", err, unsetVar)
	}
}

func TestLoadConfigHTTPConsole(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, validConfig), transportHTTP)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.stdio() {
		t.Error("stdio() = true under the HTTP transport")
	}
	if cfg.console() != os.Stdout {
		t.Error("console() is not stdout under the HTTP transport")
	}
}

func TestListensBeyondLoopback(t *testing.T) {
	tests := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:3000", false},
		{"localhost:3000", false},
		{"[::1]:3000", false},
		{"LOCALHOST:1", false},
		{":3000", true},
		{"0.0.0.0:3000", true},
		{"[::]:3000", true},
		{"10.0.0.5:3000", true},
		// Names are not resolved: an unknown one counts as beyond loopback.
		{"db.internal:3000", true},
		// Malformed: no warning here, the listen call reports it.
		{"nonsense", false},
	}
	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			if got := listensBeyondLoopback(tt.listen); got != tt.want {
				t.Errorf("listensBeyondLoopback(%q) = %v, want %v", tt.listen, got, tt.want)
			}
		})
	}
}

func TestLoadConfigUnknownTransport(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, validConfig), "websocket")
	if err == nil || !strings.Contains(err.Error(), "transport") {
		t.Errorf("err = %v, want it to mention transport", err)
	}
}

func TestLoadConfigTLS(t *testing.T) {
	certs := writeTestCerts(t)
	tests := []struct {
		name  string
		setup func(*testing.T)
		body  string
		// check inspects the derived tls.Config; nil means expect plaintext.
		check   func(t *testing.T, cfg *Config)
		wantErr []string
	}{
		{name: "absent section is plaintext", body: validConfig, check: nil},
		{name: "null section is plaintext", body: validConfig + "  tls:\n", check: nil},
		{
			// Says "tls" while choosing nothing: a misconfiguration, like
			// "masking: {}".
			name:    "empty section is an error",
			body:    validConfig + "  tls: {}\n",
			wantErr: []string{"database.tls has no settings", "enabled: true"},
		},
		{
			name: "enabled true alone is a choice",
			body: validConfig + "  tls: {enabled: true}\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.tlsConfig.RootCAs != nil {
					t.Error("want system roots")
				}
			},
		},
		{
			name: "enabled false is plaintext even with a CA",
			body: validConfig + "  tls:\n    enabled: false\n    ca: " + certs.ca + "\n",
		},
		{
			name: "enabled alone verifies against system roots and the host",
			body: validConfig + "  tls:\n    enabled: true\n",
			check: func(t *testing.T, cfg *Config) {
				tc := cfg.tlsConfig
				if tc.RootCAs != nil || tc.InsecureSkipVerify || len(tc.Certificates) != 0 {
					t.Errorf("want bare verifying config, got %+v", tc)
				}
				if tc.ServerName != "127.0.0.1" {
					t.Errorf("ServerName = %q, want database.host", tc.ServerName)
				}
				if tc.MinVersion != tls.VersionTLS12 {
					t.Errorf("MinVersion = %d, want TLS 1.2", tc.MinVersion)
				}
			},
		},
		{
			name: "a CA without enabled turns it on",
			body: validConfig + "  tls:\n    ca: " + certs.ca + "\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.tlsConfig.RootCAs == nil {
					t.Error("RootCAs not set from database.tls.ca")
				}
			},
		},
		{
			name: "server_name overrides the host",
			body: validConfig + "  tls:\n    server_name: db.internal\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.tlsConfig.ServerName != "db.internal" {
					t.Errorf("ServerName = %q", cfg.tlsConfig.ServerName)
				}
			},
		},
		{
			name: "client cert and key load as a pair",
			body: validConfig + "  tls:\n    cert: " + certs.cert + "\n    key: " + certs.key + "\n",
			check: func(t *testing.T, cfg *Config) {
				if len(cfg.tlsConfig.Certificates) != 1 {
					t.Errorf("Certificates = %d, want 1", len(cfg.tlsConfig.Certificates))
				}
			},
		},
		{
			name: "insecure_skip_verify alone is allowed",
			body: validConfig + "  tls:\n    insecure_skip_verify: true\n",
			check: func(t *testing.T, cfg *Config) {
				if !cfg.tlsConfig.InsecureSkipVerify || !cfg.tlsUnverified() {
					t.Error("InsecureSkipVerify not carried through")
				}
			},
		},
		{
			name:  "paths expand placeholders",
			setup: func(t *testing.T) { t.Setenv("MCP_TEST_CA", certs.ca) },
			body:  validConfig + "  tls:\n    ca: ${MCP_TEST_CA}\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.tlsConfig.RootCAs == nil {
					t.Error("RootCAs not set from an expanded path")
				}
			},
		},
		{
			name:    "unset placeholder in a path is named",
			body:    validConfig + "  tls:\n    ca: ${" + unsetVar + "}\n",
			wantErr: []string{unsetVar},
		},
		{
			name:    "missing CA file names the key",
			body:    validConfig + "  tls:\n    ca: /nonexistent/ca.pem\n",
			wantErr: []string{"database.tls.ca", "/nonexistent/ca.pem"},
		},
		{
			name:    "CA file without certificates",
			body:    validConfig + "  tls:\n    ca: " + certs.notPEM + "\n",
			wantErr: []string{"database.tls.ca", "no certificates"},
		},
		{
			name:    "cert without key",
			body:    validConfig + "  tls:\n    cert: " + certs.cert + "\n",
			wantErr: []string{"database.tls.cert", "database.tls.key"},
		},
		{
			name:    "key without cert",
			body:    validConfig + "  tls:\n    key: " + certs.key + "\n",
			wantErr: []string{"database.tls.cert", "database.tls.key"},
		},
		{
			name:    "bad key pair names both keys",
			body:    validConfig + "  tls:\n    cert: " + certs.cert + "\n    key: " + certs.notPEM + "\n",
			wantErr: []string{"database.tls.cert", "database.tls.key"},
		},
		{
			name:    "skip_verify with a CA is a contradiction",
			body:    validConfig + "  tls:\n    insecure_skip_verify: true\n    ca: " + certs.ca + "\n",
			wantErr: []string{"insecure_skip_verify", "database.tls.ca"},
		},
		{
			name:    "skip_verify with a server_name is a contradiction",
			body:    validConfig + "  tls:\n    insecure_skip_verify: true\n    server_name: db\n",
			wantErr: []string{"insecure_skip_verify", "database.tls.server_name"},
		},
		{
			// The pair check runs before any file is read, so a bad path
			// doesn't hide the shape error.
			name:    "disabled section skips file checks",
			body:    validConfig + "  tls:\n    enabled: false\n    ca: /nonexistent/ca.pem\n",
			wantErr: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			cfg, err := LoadConfig(writeConfig(t, tt.body), transportHTTP)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("LoadConfig succeeded, want error (tls=%+v)", cfg.tlsConfig)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if tt.check == nil {
				if cfg.tlsConfig != nil || cfg.tlsOn() {
					t.Fatalf("want plaintext, got tls config %+v", cfg.tlsConfig)
				}
				return
			}
			if cfg.tlsConfig == nil || !cfg.tlsOn() {
				t.Fatal("want a tls config, got nil")
			}
			tt.check(t, cfg)
		})
	}
}
