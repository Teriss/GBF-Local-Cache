package migration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/logging"
)

func TestOnlineMigrationCopiesAndPromotesRoot(t *testing.T) {
	parent := t.TempDir()
	oldRoot := filepath.Join(parent, "old")
	newRoot := filepath.Join(parent, "new")
	if err := os.MkdirAll(filepath.Join(oldRoot, "objects", "aa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "objects", "aa", "asset"), []byte("asset-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := cache.NewRootSet(oldRoot)
	manager := New(roots, logging.NewRing(20))
	if err := manager.Start(context.Background(), oldRoot, newRoot, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	primary, fallback := roots.Roots()
	if primary != filepath.Clean(newRoot) || fallback != "" {
		t.Fatalf("roots = %q, %q; want new primary and no fallback", primary, fallback)
	}
	data, err := os.ReadFile(filepath.Join(newRoot, "objects", "aa", "asset"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "asset-bytes" {
		t.Fatalf("copied data = %q", data)
	}
	if got := manager.Status().State; got != StateCompleted {
		t.Fatalf("migration state = %q, want completed", got)
	}
}

func TestMigrationDoesNotOverwriteNewDestination(t *testing.T) {
	parent := t.TempDir()
	oldRoot := filepath.Join(parent, "old")
	newRoot := filepath.Join(parent, "new")
	path := filepath.Join("objects", "aa", "asset")
	if err := os.MkdirAll(filepath.Join(oldRoot, filepath.Dir(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(newRoot, filepath.Dir(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, path), []byte("old-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, path), []byte("newer-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := New(cache.NewRootSet(oldRoot), logging.NewRing(20))
	if err := manager.StartWithWorker(context.Background(), context.Background(), oldRoot, newRoot, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(newRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "newer-value" {
		t.Fatalf("destination was overwritten: %q", data)
	}
	if got := manager.Status().State; got != StateCompleted {
		t.Fatalf("migration state = %q, want completed", got)
	}
}

func TestMigrationWorkerOutlivesScanContext(t *testing.T) {
	parent := t.TempDir()
	oldRoot := filepath.Join(parent, "old")
	newRoot := filepath.Join(parent, "new")
	if err := os.MkdirAll(filepath.Join(oldRoot, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "objects", "asset"), []byte("asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanContext, cancelScan := context.WithCancel(context.Background())
	manager := New(cache.NewRootSet(oldRoot), logging.NewRing(20))
	if err := manager.StartWithWorker(scanContext, context.Background(), oldRoot, newRoot, nil); err != nil {
		t.Fatal(err)
	}
	cancelScan()
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.Status().State; got != StateCompleted {
		t.Fatalf("migration state = %q, want completed", got)
	}
}
