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

func TestLoadOrCreateDefaultsCloseBehaviorForLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	if loaded.CloseBehavior != CloseBehaviorTray {
		t.Fatalf("close behavior = %q, want %q", loaded.CloseBehavior, CloseBehaviorTray)
	}
}

func TestValidateRejectsInvalidListenPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:not-a-port"} {
		cfg := Default()
		cfg.ListenAddress = address
		if err := Validate(cfg); err == nil {
			t.Fatalf("Validate() accepted invalid listen address %q", address)
		}
	}
}
