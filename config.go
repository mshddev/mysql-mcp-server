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
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		Database string `yaml:"database"`
	} `yaml:"database"`
	Limits struct {
		MaxRows          int `yaml:"max_rows"`
		TimeoutSeconds   int `yaml:"timeout_seconds"`
		MaxResponseBytes int `yaml:"max_response_bytes"`
		MaxConnections   int `yaml:"max_connections"`
	} `yaml:"limits"`
}

// LoadConfig reads the YAML file, expands ${VAR} placeholders from the
// environment, and fails fast if a referenced variable is unset.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var missing []string
	expanded := os.Expand(string(raw), func(key string) string {
		val, ok := os.LookupEnv(key)
		if !ok {
			missing = append(missing, key)
		}
		return val
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %v", missing)
	}

	cfg := &Config{}
	cfg.Server.Listen = ":3000"
	cfg.Database.Port = 3306
	cfg.Limits.MaxRows = 200
	cfg.Limits.TimeoutSeconds = 30
	cfg.Limits.MaxResponseBytes = 1 << 20
	cfg.Limits.MaxConnections = 10

	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Server.AuthToken == "" {
		return nil, fmt.Errorf("server.auth_token must not be empty")
	}
	if cfg.Database.Host == "" || cfg.Database.User == "" || cfg.Database.Database == "" {
		return nil, fmt.Errorf("database.host, database.user and database.database are required")
	}
	return cfg, nil
}
