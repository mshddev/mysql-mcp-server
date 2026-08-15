package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config that passes every validation rule, used as the starting point for
// the failure cases below.
const validConfig = `
server:
  auth_token: s3cret
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
	cfg, err := LoadConfig(writeConfig(t, validConfig))
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
}

func TestLoadConfigOverridesDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
server:
  listen: 0.0.0.0:9999
  auth_token: s3cret
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
limits:
  timeout_seconds: 5
  max_response_bytes: 1024
  max_connections: 2
`))
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
  auth_token: s3cret
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: ${MCP_TEST_DB_PASSWORD}
  dbname: mcp_dev
`))
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

	cfg, err := LoadConfig(writeConfig(t, `
server:
  listen: ${MCP_TEST_LISTEN}
  auth_token: ${MCP_TEST_TOKEN}
database:
  host: ${MCP_TEST_HOST}
  username: ${MCP_TEST_USER}
  password: ${MCP_TEST_PASSWORD}
  dbname: ${MCP_TEST_DATABASE}
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	got := []string{
		cfg.Server.Listen, cfg.Server.AuthToken, cfg.Database.Host,
		cfg.Database.Username, cfg.Database.Password, cfg.Database.DBName,
	}
	want := []string{"0.0.0.0:1234", "tok#en", "db.internal", "reader", "pw:1", "shop"}
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tt.body))
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

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*testing.T)
		body    string
		wantIn  []string
		wantOut string
	}{
		{
			name: "missing auth token",
			body: `
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"auth_token"},
		},
		{
			name:  "auth token expands to empty",
			setup: func(t *testing.T) { t.Setenv("MCP_TEST_TOKEN", "") },
			body: `
server:
  auth_token: ${MCP_TEST_TOKEN}
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"auth_token"},
		},
		{
			name: "missing host",
			body: `
server:
  auth_token: s3cret
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
  auth_token: s3cret
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
  auth_token: s3cret
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
  auth_token: ${` + unsetVar + `}
database:
  host: 127.0.0.1
  username: mcp_readonly
  password: devpassword
  dbname: mcp_dev
`,
			wantIn: []string{"unset environment variables", unsetVar},
			// The name of the variable is reported, never a partly-expanded value.
			wantOut: "auth_token must not be empty",
		},
		{
			name: "several unset environment variables",
			body: `
server:
  auth_token: ${` + unsetVar + `_A}
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
			body:   "server:\n  auth_token: [unclosed\n",
			wantIn: []string{"parse config"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			cfg, err := LoadConfig(writeConfig(t, tt.body))
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
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
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
