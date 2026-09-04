package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

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
