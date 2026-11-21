package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadTestConfigYAML(t *testing.T, yamlBody string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestMediaTempURLValidation(t *testing.T) {
	// Enabled without gateway.public_url must fail validation.
	_, err := loadTestConfigYAML(t, `
version: "1"
media:
  backend: fs
  temp_url:
    enabled: true
`)
	if err == nil || !strings.Contains(err.Error(), "gateway.public_url") {
		t.Fatalf("expected gateway.public_url validation error, got: %v", err)
	}

	// Path without trailing slash must fail.
	_, err = loadTestConfigYAML(t, `
version: "1"
gateway:
  public_url: https://bot.example.com
media:
  backend: fs
  temp_url:
    enabled: true
    path: /media/t
`)
	if err == nil || !strings.Contains(err.Error(), "media.temp_url.path") {
		t.Fatalf("expected media.temp_url.path error, got: %v", err)
	}

	// Valid temp_url + retention + derived_ttl + unified files default (Option 1B).
	cfg, err := loadTestConfigYAML(t, `
version: "1"
gateway:
  public_url: https://bot.example.com
media:
  backend: fs
  retention: 72h
  derived_ttl: 12h
  temp_url:
    enabled: true
    path: /media/tmp/
    ttl: 15m
    secret: top-secret-key
`)
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if cfg.Media.TempURL.MediaTempURLPath() != "/media/tmp/" {
		t.Fatalf("unexpected temp_url path: %q", cfg.Media.TempURL.MediaTempURLPath())
	}
	if cfg.Media.TempURL.MediaTempURLTTL() != 15*time.Minute {
		t.Fatalf("unexpected temp_url ttl: %v", cfg.Media.TempURL.MediaTempURLTTL())
	}
	if cfg.Media.MediaRetention() != 72*time.Hour {
		t.Fatalf("unexpected retention: %v", cfg.Media.MediaRetention())
	}
	if cfg.Media.MediaDerivedTTL() != 12*time.Hour {
		t.Fatalf("unexpected derived_ttl: %v", cfg.Media.MediaDerivedTTL())
	}
	if cfg.Files.HasExplicitStore() {
		t.Fatal("expected Files.HasExplicitStore() == false when files backend/dir/s3 are unset")
	}
}

func TestFilesHasExplicitStore(t *testing.T) {
	if (FilesConfig{}).HasExplicitStore() {
		t.Fatal("empty FilesConfig should not report explicit store")
	}
	if !(FilesConfig{Dir: "./custom-files"}).HasExplicitStore() {
		t.Fatal("FilesConfig with Dir should report explicit store")
	}
	if !(FilesConfig{Backend: "s3"}).HasExplicitStore() {
		t.Fatal("FilesConfig with Backend should report explicit store")
	}
	if !(FilesConfig{S3: MediaS3{Bucket: "b"}}).HasExplicitStore() {
		t.Fatal("FilesConfig with S3.Bucket should report explicit store")
	}
}
