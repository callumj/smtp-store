package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesDefaultsAndBuildsUserMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `storage_root: ./captures
users:
  - username: Camera
    password: secret
`)

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
	if got := cfg.UserMap()["camera"]; got != "secret" {
		t.Fatalf("unexpected user map password: %q", got)
	}
}

func TestLoadFailsWithoutStorageRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	writeFile(t, configPath, `users:
  - username: camera
    password: secret
`)

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
`)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected tls file validation error")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}
