package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultListenAddr = "127.0.0.1:2525"
const defaultWebListenAddr = "0.0.0.0:8080"
const defaultWebSessionTTL = "168h"

// Config is the runtime configuration for the SMTP capture service.
type Config struct {
	ListenAddr    string      `yaml:"listen_addr"`
	StorageRoot   string      `yaml:"storage_root"`
	Hostname      string      `yaml:"hostname"`
	VerboseLogs   bool        `yaml:"verbose_logs"`
	TLS           TLSConfig   `yaml:"tls"`
	Web           WebConfig   `yaml:"web"`
	Users         []UserCreds `yaml:"users"`
	UIUsers       []UserCreds `yaml:"ui_users"`
	webSessionTTL time.Duration
}

// TLSConfig controls optional STARTTLS support.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// WebConfig controls the embedded web UI server.
type WebConfig struct {
	ListenAddr    string `yaml:"listen_addr"`
	SessionTTL    string `yaml:"session_ttl"`
	SessionSecret string `yaml:"session_secret"`
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
	if strings.TrimSpace(c.Web.ListenAddr) == "" {
		c.Web.ListenAddr = defaultWebListenAddr
	}
	if strings.TrimSpace(c.Web.SessionTTL) == "" {
		c.Web.SessionTTL = defaultWebSessionTTL
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
	if len(c.UIUsers) == 0 {
		return errors.New("at least one ui_user is required")
	}
	if strings.TrimSpace(c.Web.SessionSecret) == "" {
		return errors.New("web.session_secret is required")
	}

	ttl, err := time.ParseDuration(c.Web.SessionTTL)
	if err != nil {
		return fmt.Errorf("invalid web.session_ttl: %w", err)
	}
	if ttl <= 0 {
		return errors.New("web.session_ttl must be > 0")
	}
	c.webSessionTTL = ttl

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

	uiSeen := make(map[string]struct{}, len(c.UIUsers))
	for i, u := range c.UIUsers {
		if strings.TrimSpace(u.Username) == "" {
			return fmt.Errorf("ui_users[%d].username is required", i)
		}
		if strings.TrimSpace(u.Password) == "" {
			return fmt.Errorf("ui_users[%d].password is required", i)
		}
		key := strings.ToLower(strings.TrimSpace(u.Username))
		if _, ok := uiSeen[key]; ok {
			return fmt.Errorf("duplicate ui username: %q", u.Username)
		}
		uiSeen[key] = struct{}{}
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

// UIUserMap returns UI auth users keyed by lowercased username.
func (c *Config) UIUserMap() map[string]string {
	out := make(map[string]string, len(c.UIUsers))
	for _, u := range c.UIUsers {
		out[strings.ToLower(strings.TrimSpace(u.Username))] = u.Password
	}
	return out
}

// WebSessionTTLDuration returns parsed web.session_ttl.
func (c *Config) WebSessionTTLDuration() time.Duration {
	if c.webSessionTTL > 0 {
		return c.webSessionTTL
	}
	ttl, err := time.ParseDuration(strings.TrimSpace(c.Web.SessionTTL))
	if err == nil && ttl > 0 {
		return ttl
	}
	fallback, _ := time.ParseDuration(defaultWebSessionTTL)
	return fallback
}
