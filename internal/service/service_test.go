package service

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gbf-local-cache/internal/config"
	"gbf-local-cache/internal/migration"
)

func TestServiceLifecycle(t *testing.T) {
	cfg := config.Default()
	// Let the OS pick a free port while preserving the loopback-only policy.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	cfg.ListenAddress = address

	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := svc.Snapshot().State; got != StateRunning {
		t.Fatalf("state after Start = %q, want %q", got, StateRunning)
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := svc.Snapshot().State; got != StateStopped {
		t.Fatalf("state after Stop = %q, want %q", got, StateStopped)
	}
}

func TestChangeCacheRootWhileStoppedRenamesSameVolume(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-cache")
	newRoot := filepath.Join(base, "new-cache")
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(oldRoot, "objects", "marker.bin")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.CacheRoot = oldRoot
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangeCacheRoot(context.Background(), newRoot); err != nil {
		t.Fatalf("ChangeCacheRoot() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "objects", "marker.bin")); err != nil {
		t.Fatalf("moved cache marker: %v", err)
	}
	if _, err := os.Stat(oldRoot); !os.IsNotExist(err) {
		t.Fatalf("old cache root still exists, stat error = %v", err)
	}
	if got := svc.Snapshot().CacheRoot; got != newRoot {
		t.Fatalf("snapshot cache root = %q, want %q", got, newRoot)
	}
}

func TestChangeCacheRootWhileStoppedCopiesExistingTarget(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "old-cache")
	newRoot := filepath.Join(base, "new-cache")
	if err := os.MkdirAll(filepath.Join(oldRoot, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(oldRoot, "objects", "marker.bin")
	if err := os.WriteFile(marker, []byte("copied"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.CacheRoot = oldRoot
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangeCacheRoot(context.Background(), newRoot); err != nil {
		t.Fatalf("ChangeCacheRoot() error = %v", err)
	}
	svc.mu.RLock()
	migrator := svc.migration
	svc.mu.RUnlock()
	if migrator == nil {
		t.Fatal("stopped migration did not create a migration manager")
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := migrator.Wait(waitContext); err != nil {
		t.Fatalf("migration did not finish: %v", err)
	}
	if got := svc.Snapshot().Migration.State; got != migration.StateCompleted {
		t.Fatalf("migration state = %q, want %q", got, migration.StateCompleted)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "objects", "marker.bin")); err != nil {
		t.Fatalf("copied cache marker: %v", err)
	}
	if got := svc.Snapshot().CacheRoot; got != newRoot {
		t.Fatalf("snapshot cache root = %q, want %q", got, newRoot)
	}
}
