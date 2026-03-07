package config

import (
	"os"
	"path/filepath"
	"testing"
)

const requiredConfigBlock = `
web:
  session_secret: test-secret
ui_users:
  - username: ui
    password: ui-secret
`

func TestLoadAppliesDefaultsAndBuildsUserMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `storage_root: ./captures
users:
  - username: Camera
    password: secret
`+requiredConfigBlock)

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:2525" {
		t.Fatalf("unexpected default listen addr: %q", cfg.ListenAddr)
	}
	if cfg.Hostname == "" {
		t.Fatal("expected hostname default")
	}
	if cfg.Web.ListenAddr != "0.0.0.0:8080" {
		t.Fatalf("unexpected default web listen addr: %q", cfg.Web.ListenAddr)
	}
	if cfg.WebSessionTTLDuration().String() != "168h0m0s" {
		t.Fatalf("unexpected default web session ttl: %s", cfg.WebSessionTTLDuration())
	}
	if got := cfg.UserMap()["camera"]; got != "secret" {
		t.Fatalf("unexpected user map password: %q", got)
	}
	if got := cfg.UIUserMap()["ui"]; got != "ui-secret" {
		t.Fatalf("unexpected ui user map password: %q", got)
	}
}

func TestLoadFailsWithoutStorageRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `users:
  - username: camera
    password: secret
`+requiredConfigBlock)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadValidatesTLSFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `storage_root: ./captures
users:
  - username: camera
    password: secret
tls:
  enabled: true
  cert_file: ./missing.crt
  key_file: ./missing.key
`+requiredConfigBlock)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected tls file validation error")
	}
}

func TestLoadFailsWithoutUIUsers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `storage_root: ./captures
users:
  - username: camera
    password: secret
web:
  session_secret: test-secret
`)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected ui_users validation error")
	}
}

func TestLoadFailsWithoutWebSessionSecret(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `storage_root: ./captures
users:
  - username: camera
    password: secret
ui_users:
  - username: ui
    password: ui-secret
`)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected web.session_secret validation error")
	}
}

func TestLoadFailsWithInvalidSessionTTL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `storage_root: ./captures
users:
  - username: camera
    password: secret
web:
  session_secret: test-secret
  session_ttl: nope
ui_users:
  - username: ui
    password: ui-secret
`)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected web.session_ttl validation error")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}
