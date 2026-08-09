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
const defaultClassificationProvider = "gemini"
const defaultClassificationConfidenceThreshold = 0.60
const defaultClassificationWorkerConcurrency = 1
const defaultClassificationFrameCount = 6
const defaultClassificationBackfillWindow = "168h"
const defaultClassificationRetryMax = 3
const defaultMQTTPort = 1883
const defaultMQTTClientID = "smtp-store"
const defaultMQTTTopicPrefix = "smtp-store"
const defaultMQTTDiscoveryPrefix = "homeassistant"
const defaultMQTTQoS = 1
const defaultMQTTMotionResetAfter = "60s"

// Config is the runtime configuration for the SMTP capture service.
type Config struct {
	ListenAddr                      string               `yaml:"listen_addr"`
	StorageRoot                     string               `yaml:"storage_root"`
	IndexPath                       string               `yaml:"index_path"`
	Hostname                        string               `yaml:"hostname"`
	VerboseLogs                     bool                 `yaml:"verbose_logs"`
	TLS                             TLSConfig            `yaml:"tls"`
	Web                             WebConfig            `yaml:"web"`
	Classification                  ClassificationConfig `yaml:"classification"`
	MQTT                            MQTTConfig           `yaml:"mqtt"`
	Users                           []UserCreds          `yaml:"users"`
	UIUsers                         []UserCreds          `yaml:"ui_users"`
	webSessionTTL                   time.Duration
	classificationBackfillWindow    time.Duration
	mqttMotionResetAfter            time.Duration
	classificationEnabled           bool
	classificationStoreRawResponses bool
	mqttEnabled                     bool
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

// ClassificationConfig controls async video detection metadata generation.
type ClassificationConfig struct {
	Enabled             *bool   `yaml:"enabled"`
	Provider            string  `yaml:"provider"`
	Model               string  `yaml:"model"`
	APIKey              string  `yaml:"api_key"`
	ConfidenceThreshold float64 `yaml:"confidence_threshold"`
	WorkerConcurrency   int     `yaml:"worker_concurrency"`
	FrameCount          int     `yaml:"frame_count"`
	BackfillWindow      string  `yaml:"backfill_window"`
	RetryMax            int     `yaml:"retry_max"`
	StoreRawResponse    *bool   `yaml:"store_raw_response"`
}

// MQTTConfig controls Home Assistant MQTT discovery and event publishing.
type MQTTConfig struct {
	Enabled          *bool    `yaml:"enabled"`
	Host             string   `yaml:"host"`
	Port             int      `yaml:"port"`
	Username         string   `yaml:"username"`
	Password         string   `yaml:"password"`
	ClientID         string   `yaml:"client_id"`
	TopicPrefix      string   `yaml:"topic_prefix"`
	DiscoveryPrefix  string   `yaml:"discovery_prefix"`
	QoS              byte     `yaml:"qos"`
	MotionResetAfter string   `yaml:"motion_reset_after"`
	PublicBaseURL    string   `yaml:"public_base_url"`
	MediaToken       string   `yaml:"media_token"`
	NotifyCategories []string `yaml:"notify_categories"`
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
	c.classificationEnabled = true
	if c.Classification.Enabled != nil {
		c.classificationEnabled = *c.Classification.Enabled
	}
	c.classificationStoreRawResponses = true
	if c.Classification.StoreRawResponse != nil {
		c.classificationStoreRawResponses = *c.Classification.StoreRawResponse
	}
	c.mqttEnabled = false
	if c.MQTT.Enabled != nil {
		c.mqttEnabled = *c.MQTT.Enabled
	}

	if strings.TrimSpace(c.ListenAddr) == "" {
		c.ListenAddr = defaultListenAddr
	}
	if strings.TrimSpace(c.Web.ListenAddr) == "" {
		c.Web.ListenAddr = defaultWebListenAddr
	}
	if strings.TrimSpace(c.Web.SessionTTL) == "" {
		c.Web.SessionTTL = defaultWebSessionTTL
	}
	if strings.TrimSpace(c.Classification.Provider) == "" {
		c.Classification.Provider = defaultClassificationProvider
	}
	if c.Classification.ConfidenceThreshold == 0 {
		c.Classification.ConfidenceThreshold = defaultClassificationConfidenceThreshold
	}
	if c.Classification.WorkerConcurrency == 0 {
		c.Classification.WorkerConcurrency = defaultClassificationWorkerConcurrency
	}
	if c.Classification.FrameCount == 0 {
		c.Classification.FrameCount = defaultClassificationFrameCount
	}
	if strings.TrimSpace(c.Classification.BackfillWindow) == "" {
		c.Classification.BackfillWindow = defaultClassificationBackfillWindow
	}
	if c.Classification.RetryMax == 0 {
		c.Classification.RetryMax = defaultClassificationRetryMax
	}
	if strings.TrimSpace(c.Hostname) == "" {
		hostname, err := os.Hostname()
		if err != nil || strings.TrimSpace(hostname) == "" {
			hostname = "localhost"
		}
		c.Hostname = hostname
	}
	if c.MQTT.Port == 0 {
		c.MQTT.Port = defaultMQTTPort
	}
	if strings.TrimSpace(c.MQTT.ClientID) == "" {
		c.MQTT.ClientID = defaultMQTTClientID
	}
	if strings.TrimSpace(c.MQTT.TopicPrefix) == "" {
		c.MQTT.TopicPrefix = defaultMQTTTopicPrefix
	}
	if strings.TrimSpace(c.MQTT.DiscoveryPrefix) == "" {
		c.MQTT.DiscoveryPrefix = defaultMQTTDiscoveryPrefix
	}
	if c.MQTT.QoS == 0 {
		c.MQTT.QoS = defaultMQTTQoS
	}
	if strings.TrimSpace(c.MQTT.MotionResetAfter) == "" {
		c.MQTT.MotionResetAfter = defaultMQTTMotionResetAfter
	}
	if len(c.MQTT.NotifyCategories) == 0 {
		c.MQTT.NotifyCategories = []string{"person", "animal", "vehicle"}
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

	if c.Classification.WorkerConcurrency < 1 {
		return errors.New("classification.worker_concurrency must be >= 1")
	}
	if c.Classification.FrameCount < 1 {
		return errors.New("classification.frame_count must be >= 1")
	}
	if c.Classification.RetryMax < 0 {
		return errors.New("classification.retry_max must be >= 0")
	}
	if c.Classification.ConfidenceThreshold < 0 || c.Classification.ConfidenceThreshold > 1 {
		return errors.New("classification.confidence_threshold must be between 0 and 1")
	}
	backfillWindow, err := time.ParseDuration(strings.TrimSpace(c.Classification.BackfillWindow))
	if err != nil {
		return fmt.Errorf("invalid classification.backfill_window: %w", err)
	}
	if backfillWindow <= 0 {
		return errors.New("classification.backfill_window must be > 0")
	}
	c.classificationBackfillWindow = backfillWindow

	if c.classificationEnabled {
		if strings.TrimSpace(c.Classification.Provider) == "" {
			return errors.New("classification.provider is required when classification.enabled=true")
		}
		if strings.TrimSpace(c.Classification.Model) == "" {
			return errors.New("classification.model is required when classification.enabled=true")
		}
		if strings.TrimSpace(c.Classification.APIKey) == "" {
			return errors.New("classification.api_key is required when classification.enabled=true")
		}
	}

	mqttMotionResetAfter, err := time.ParseDuration(strings.TrimSpace(c.MQTT.MotionResetAfter))
	if err != nil {
		return fmt.Errorf("invalid mqtt.motion_reset_after: %w", err)
	}
	if mqttMotionResetAfter <= 0 {
		return errors.New("mqtt.motion_reset_after must be > 0")
	}
	c.mqttMotionResetAfter = mqttMotionResetAfter
	if c.MQTT.Port < 1 || c.MQTT.Port > 65535 {
		return errors.New("mqtt.port must be between 1 and 65535")
	}
	if c.MQTT.QoS > 2 {
		return errors.New("mqtt.qos must be 0, 1, or 2")
	}
	if c.mqttEnabled {
		if strings.TrimSpace(c.MQTT.Host) == "" {
			return errors.New("mqtt.host is required when mqtt.enabled=true")
		}
		if strings.TrimSpace(c.MQTT.PublicBaseURL) == "" {
			return errors.New("mqtt.public_base_url is required when mqtt.enabled=true")
		}
		if strings.TrimSpace(c.MQTT.MediaToken) == "" {
			return errors.New("mqtt.media_token is required when mqtt.enabled=true")
		}
	}
	for i, category := range c.MQTT.NotifyCategories {
		if strings.TrimSpace(category) == "" {
			return fmt.Errorf("mqtt.notify_categories[%d] is required", i)
		}
		c.MQTT.NotifyCategories[i] = strings.ToLower(strings.TrimSpace(category))
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

// ClassificationEnabled returns whether async classification should run.
func (c *Config) ClassificationEnabled() bool {
	return c.classificationEnabled
}

// ClassificationBackfillWindowDuration returns parsed backfill window duration.
func (c *Config) ClassificationBackfillWindowDuration() time.Duration {
	if c.classificationBackfillWindow > 0 {
		return c.classificationBackfillWindow
	}
	fallback, _ := time.ParseDuration(defaultClassificationBackfillWindow)
	return fallback
}

// ClassificationStoreRawResponseEnabled returns whether raw model output is persisted.
func (c *Config) ClassificationStoreRawResponseEnabled() bool {
	return c.classificationStoreRawResponses
}

// MQTTEnabled returns whether Home Assistant MQTT publishing should run.
func (c *Config) MQTTEnabled() bool {
	return c.mqttEnabled
}

// IndexEnabled reports whether the local metadata index should be used.
func (c *Config) IndexEnabled() bool {
	return strings.TrimSpace(c.IndexPath) != ""
}

// MQTTMotionResetAfterDuration returns parsed mqtt.motion_reset_after.
func (c *Config) MQTTMotionResetAfterDuration() time.Duration {
	if c.mqttMotionResetAfter > 0 {
		return c.mqttMotionResetAfter
	}
	fallback, _ := time.ParseDuration(defaultMQTTMotionResetAfter)
	return fallback
}
