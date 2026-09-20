package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RootMarkerName identifies a directory as a GBF Local Cache root. Cache
// operations are deliberately confined to the managed data directories even
// when the user selects a directory that already contains unrelated files.
const RootMarkerName = ".gbf-local-cache"

const (
	rootMarkerFormat = 1
	rootMarkerApp    = "GBF Local Cache"
)

type rootMarker struct {
	Format      int    `json:"format"`
	Application string `json:"application"`
}

// ManagedDataDirectories returns the only root subdirectories that contain
// cache data. State and the root marker are metadata owned by the application,
// not cache objects and must never be included in migration or size totals.
func ManagedDataDirectories() []string {
	return []string{"objects", "metadata"}
}

// EnsureRoot creates and validates the managed cache-root layout. A missing
// marker is treated as a legacy cache root and is claimed safely by creating
// the marker exclusively. An existing marker with unknown contents is rejected
// so the application never silently takes ownership of another application's
// directory.
func EnsureRoot(root string) error {
	if root == "" {
		return errors.New("cache root is empty")
	}
	absolute, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cache root is not a directory: %s", absolute)
	}

	markerPath := filepath.Join(absolute, RootMarkerName)
	if err := ensureMarker(markerPath); err != nil {
		return err
	}
	for _, directory := range append(ManagedDataDirectories(), "state") {
		path := filepath.Join(absolute, directory)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("create cache directory %s: %w", directory, err)
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return fmt.Errorf("inspect cache directory %s: %w", directory, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("cache directory is not a real directory: %s", directory)
		}
	}
	return nil
}

func ensureMarker(path string) error {
	marker := rootMarker{Format: rootMarkerFormat, Application: rootMarkerApp}
	payload, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := file.Write(payload); writeErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return writeErr
		}
		if syncErr := file.Sync(); syncErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return syncErr
		}
		return file.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create cache root marker: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("cache root marker is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read cache root marker: %w", err)
	}
	var existing rootMarker
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("decode cache root marker: %w", err)
	}
	if existing.Format != rootMarkerFormat || existing.Application != rootMarkerApp {
		return fmt.Errorf("cache root marker belongs to an incompatible application")
	}
	return nil
}
