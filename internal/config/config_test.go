package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()

	if err := SaveAtomic(path, cfg); err != nil {
		t.Fatalf("SaveAtomic() error = %v", err)
	}
	got, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	if got.ListenAddress != cfg.ListenAddress || got.NetworkMode != cfg.NetworkMode {
		t.Fatalf("loaded config = %#v, want %#v", got, cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, ".config-does-not-exist.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file unexpectedly exists")
	}
}

func TestValidateRejectsPublicBind(t *testing.T) {
	cfg := Default()
	cfg.ListenAddress = "0.0.0.0:8124"
	if err := Validate(cfg); err == nil {
		t.Fatal("Validate() accepted a public bind address")
	}
}
