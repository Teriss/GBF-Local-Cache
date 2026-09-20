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
