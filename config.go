package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
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
	// Masking is opt-in per deployment: an absent section means off.
	Masking *MaskingConfig `yaml:"masking"`

	// masker is derived from Masking at load time; nil when masking is off.
	masker *Masker
}

type MaskingConfig struct {
	// Enabled is a pointer so "omitted" is distinguishable from "false":
	// omitted with rules present means true.
	Enabled *bool    `yaml:"enabled"`
	Mask    []string `yaml:"mask"`
	Except  []string `yaml:"except"`
	// Strict turns on query-parsing enforcement: the server traces each result
	// column back to its origin instead of trusting the wire tag, closing the
	// derived-table/CTE/UNION/computed-column leaks that light masking has.
	Strict bool `yaml:"strict"`
}

// LoadConfig reads the YAML file, then expands ${VAR} placeholders from the
// environment. Expansion happens after parsing so secret values containing
// YAML-significant characters (#, :, quotes) can't corrupt the document.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	cfg.Server.Listen = "127.0.0.1:3000"
	cfg.Database.Port = 3306
	cfg.Limits.TimeoutSeconds = 30
	cfg.Limits.MaxResponseBytes = 500 << 10
	cfg.Limits.MaxConnections = 10

	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var missing []string
	for _, f := range []*string{
		&cfg.Server.Listen, &cfg.Server.AuthToken,
		&cfg.Database.Host, &cfg.Database.Username,
		&cfg.Database.Password, &cfg.Database.DBName,
	} {
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

	if cfg.Server.AuthToken == "" {
		return nil, fmt.Errorf("server.auth_token must not be empty")
	}
	if cfg.Database.Host == "" || cfg.Database.Username == "" || cfg.Database.DBName == "" {
		return nil, fmt.Errorf("database.host, database.username and database.dbname are required")
	}
	if cfg.Limits.TimeoutSeconds < 1 || cfg.Limits.MaxConnections < 1 || cfg.Limits.MaxResponseBytes < 1 {
		return nil, fmt.Errorf("limits must all be at least 1 (timeout_seconds=%d, max_connections=%d, max_response_bytes=%d)",
			cfg.Limits.TimeoutSeconds, cfg.Limits.MaxConnections, cfg.Limits.MaxResponseBytes)
	}
	if cfg.masker, err = NewMasker(cfg.Masking); err != nil {
		return nil, err
	}
	return cfg, nil
}
