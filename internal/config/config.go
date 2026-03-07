package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultListenAddr = "127.0.0.1:2525"

// Config is the runtime configuration for the SMTP capture service.
type Config struct {
	ListenAddr  string      `yaml:"listen_addr"`
	StorageRoot string      `yaml:"storage_root"`
	Hostname    string      `yaml:"hostname"`
	VerboseLogs bool        `yaml:"verbose_logs"`
	TLS         TLSConfig   `yaml:"tls"`
	Users       []UserCreds `yaml:"users"`
}

// TLSConfig controls optional STARTTLS support.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// UserCreds is a static local auth user.
type UserCreds struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Load parses YAML config from path and validates defaults and constraints.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.ListenAddr) == "" {
		c.ListenAddr = defaultListenAddr
	}
	if strings.TrimSpace(c.Hostname) == "" {
		hostname, err := os.Hostname()
		if err != nil || strings.TrimSpace(hostname) == "" {
			hostname = "localhost"
		}
		c.Hostname = hostname
	}
}

// Validate checks runtime config consistency.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.StorageRoot) == "" {
		return errors.New("storage_root is required")
	}
	if len(c.Users) == 0 {
		return errors.New("at least one user is required")
	}

	seen := make(map[string]struct{}, len(c.Users))
	for i, u := range c.Users {
		if strings.TrimSpace(u.Username) == "" {
			return fmt.Errorf("users[%d].username is required", i)
		}
		if strings.TrimSpace(u.Password) == "" {
			return fmt.Errorf("users[%d].password is required", i)
		}
		key := strings.ToLower(strings.TrimSpace(u.Username))
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate username: %q", u.Username)
		}
		seen[key] = struct{}{}
	}

	if c.TLS.Enabled {
		if strings.TrimSpace(c.TLS.CertFile) == "" {
			return errors.New("tls.cert_file is required when tls.enabled=true")
		}
		if strings.TrimSpace(c.TLS.KeyFile) == "" {
			return errors.New("tls.key_file is required when tls.enabled=true")
		}
		if _, err := os.Stat(c.TLS.CertFile); err != nil {
			return fmt.Errorf("tls.cert_file not accessible: %w", err)
		}
		if _, err := os.Stat(c.TLS.KeyFile); err != nil {
			return fmt.Errorf("tls.key_file not accessible: %w", err)
		}
	}

	return nil
}

// UserMap returns auth users keyed by lowercased username.
func (c *Config) UserMap() map[string]string {
	out := make(map[string]string, len(c.Users))
	for _, u := range c.Users {
		out[strings.ToLower(strings.TrimSpace(u.Username))] = u.Password
	}
	return out
}
