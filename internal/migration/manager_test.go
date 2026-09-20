package migration

import (
	"context"
	"encoding/json"
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

func TestMigrationRejectsInvalidExistingCachePair(t *testing.T) {
	parent := t.TempDir()
	oldRoot := filepath.Join(parent, "old")
	newRoot := filepath.Join(parent, "new")
	hashPath := filepath.Join("aa", "asset")
	for _, root := range []string{oldRoot, newRoot} {
		for _, directory := range []string{"objects/aa", "metadata/aa"} {
			if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "objects", hashPath), []byte("valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	validEntry := cache.CacheEntry{Version: cache.MetadataVersion, ContentLength: 5, StatusCode: 200}
	validMetadata, err := validEntry.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "metadata", hashPath+".json"), validMetadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "objects", hashPath), []byte("broken-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidEntry := cache.CacheEntry{Version: cache.MetadataVersion, ContentLength: 999, StatusCode: 200}
	invalidMetadata, err := invalidEntry.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "metadata", hashPath+".json"), invalidMetadata, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := New(cache.NewRootSet(oldRoot), logging.NewRing(20))
	if err := manager.Start(context.Background(), oldRoot, newRoot, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.Status().State; got != StateError {
		t.Fatalf("migration state = %q, want error", got)
	}
}

func TestCompletedRootReconcilesOnlyMatchingCompletedState(t *testing.T) {
	parent := t.TempDir()
	oldRoot := filepath.Join(parent, "old")
	newRoot := filepath.Join(parent, "new")
	if err := os.MkdirAll(filepath.Join(oldRoot, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(persistedState{Status: Status{
		State:   StateCompleted,
		OldRoot: oldRoot,
		NewRoot: newRoot,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "state", "migration.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := CompletedRoot(oldRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != filepath.Clean(newRoot) {
		t.Fatalf("CompletedRoot() = %q, %v; want %q, true", got, ok, filepath.Clean(newRoot))
	}

	if err := os.WriteFile(filepath.Join(oldRoot, "state", "migration.json"), []byte(`{"status":{"state":"running"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := CompletedRoot(oldRoot); err != nil || ok {
		t.Fatalf("running migration state reconciled: ok=%v err=%v", ok, err)
	}
}
