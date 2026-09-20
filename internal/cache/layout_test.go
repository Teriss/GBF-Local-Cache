package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRootClaimsLegacyLayoutAndPreservesUnmanagedFiles(t *testing.T) {
	root := t.TempDir()
	unmanaged := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(unmanaged, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged file disappeared: %v", err)
	}
	for _, path := range append(ManagedDataDirectories(), "state", RootMarkerName) {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("managed path %s was not created: %v", path, err)
		}
	}
	if err := EnsureRoot(root); err != nil {
		t.Fatalf("second EnsureRoot() failed: %v", err)
	}
}

func TestEnsureRootRejectsIncompatibleMarker(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, RootMarkerName)
	if err := os.WriteFile(marker, []byte(`{"format":99,"application":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoot(root); err == nil {
		t.Fatal("incompatible cache root marker was accepted")
	}
}
