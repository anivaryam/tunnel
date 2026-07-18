package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds the client configuration.
type Config struct {
	ServerURL string `yaml:"server_url"`
	AuthToken string `yaml:"auth_token"`
}

// DefaultPath returns the default config file path (~/.tunnel/config.yml).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tunnel", "config.yml")
}

// Load reads the config from the given path. Returns a zero-value Config if the file doesn't exist.
// An empty path (e.g. DefaultPath() when the home dir cannot be resolved) is an error
// rather than a silent read of the process working directory.
func Load(path string) (Config, error) {
	var cfg Config
	if path == "" {
		return cfg, fmt.Errorf("config path is empty (cannot resolve home directory)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Save writes the config to the given path, creating directories as needed.
func Save(path string, cfg Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty (cannot resolve home directory)")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	// os.WriteFile honours the mode only when creating; existing files keep
	// their previous mode. Force 0600 so an upgrade from a pre-1.7 binary
	// (which wrote 0644) tightens permissions on next save.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	return nil
}
