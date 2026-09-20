package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RootSet struct {
	mu       sync.RWMutex
	primary  string
	fallback string
	version  uint64
}

func NewRootSet(primary string) *RootSet {
	return &RootSet{primary: filepath.Clean(primary), version: 1}
}

func (r *RootSet) Roots() (primary, fallback string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.primary, r.fallback
}

func (r *RootSet) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

func (r *RootSet) BeginMigration(newRoot, oldRoot string) {
	r.mu.Lock()
	r.primary = filepath.Clean(newRoot)
	r.fallback = filepath.Clean(oldRoot)
	r.version++
	r.mu.Unlock()
}

func (r *RootSet) FinishMigration() {
	r.mu.Lock()
	r.fallback = ""
	r.version++
	r.mu.Unlock()
}

func (r *RootSet) RestoreFallback() {
	r.mu.Lock()
	if r.fallback != "" {
		r.primary, r.fallback = r.fallback, r.primary
		r.version++
	}
	r.mu.Unlock()
}

type DiskStore struct {
	roots       *RootSet
	writes      sync.WaitGroup
	writeSlots  chan struct{}
	sizeMu      sync.Mutex
	cachedSize  int64
	sizeVersion uint64
	sizeReady   bool
}

type HealthReport struct {
	CheckedAt    time.Time `json:"checked_at"`
	Files        int64     `json:"files"`
	Bytes        int64     `json:"bytes"`
	ValidFiles   int64     `json:"valid_files"`
	InvalidFiles int64     `json:"invalid_files"`
	InvalidBytes int64     `json:"invalid_bytes"`
	Roots        []string  `json:"roots"`
}

func NewDiskStore(roots *RootSet) *DiskStore {
	if roots == nil {
		roots = NewRootSet("")
	}
	store := &DiskStore{roots: roots, writeSlots: make(chan struct{}, 4)}
	store.RefreshSize()
	return store
}

func (s *DiskStore) Roots() *RootSet {
	return s.roots
}

func (s *DiskStore) Read(hash string) (CacheEntry, []byte, string, error) {
	if !validHash(hash) {
		return CacheEntry{}, nil, "", fmt.Errorf("invalid cache hash %q", hash)
	}
	primary, fallback := s.roots.Roots()
	for _, root := range []string{primary, fallback} {
		if root == "" {
			continue
		}
		entry, body, err := readObject(root, hash)
		if err == nil {
			return entry, body, root, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			// A corrupt primary should not prevent a valid fallback during an
			// online migration, but it is still reported if no root succeeds.
			continue
		}
	}
	return CacheEntry{}, nil, "", os.ErrNotExist
}

func (s *DiskStore) PutAsync(hash string, entry CacheEntry, body []byte) {
	bodyCopy := append([]byte(nil), body...)
	s.writes.Add(1)
	go func() {
		defer s.writes.Done()
		s.writeSlots <- struct{}{}
		defer func() { <-s.writeSlots }()
		_ = s.Put(hash, entry, bodyCopy)
	}()
}

func (s *DiskStore) Put(hash string, entry CacheEntry, body []byte) error {
	if !validHash(hash) {
		return fmt.Errorf("invalid cache hash %q", hash)
	}
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	primary, _ := s.roots.Roots()
	if primary == "" {
		return errors.New("cache root is empty")
	}
	version := s.roots.Version()
	before := cacheFileSize(primary, hash)
	if err := writeObject(primary, hash, entry, body); err != nil {
		s.sizeReady = false
		return err
	}
	after := cacheFileSize(primary, hash)
	if s.sizeReady && s.sizeVersion == version && s.roots.Version() == version {
		s.cachedSize += after - before
	} else {
		s.sizeReady = false
	}
	return nil
}

func (s *DiskStore) Promote(hash string, entry CacheEntry, body []byte) {
	s.PutAsync(hash, entry, body)
}

func (s *DiskStore) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		s.writes.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *DiskStore) Clear() error {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	primary, fallback := s.roots.Roots()
	for _, root := range []string{primary, fallback} {
		if root == "" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, "objects")); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(root, "metadata")); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(root, "objects"), 0o700); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(root, "metadata"), 0o700); err != nil {
			return err
		}
	}
	s.sizeReady = false
	return nil
}

// DiskSize returns a cached total. The initial value is calculated once when
// the store is created; writes adjust it incrementally and root-set changes
// invalidate it. This keeps frequent UI snapshots from walking the whole
// cache tree.
func (s *DiskStore) DiskSize() int64 {
	version := s.roots.Version()
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	if !s.sizeReady || s.sizeVersion != version {
		s.cachedSize = s.calculateSize()
		s.sizeVersion = s.roots.Version()
		s.sizeReady = true
	}
	return s.cachedSize
}

// RefreshSize performs an explicit full recalculation. It is useful after an
// external migration worker has copied files outside DiskStore.Put.
func (s *DiskStore) RefreshSize() {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	s.cachedSize = s.calculateSize()
	s.sizeVersion = s.roots.Version()
	s.sizeReady = true
}

func (s *DiskStore) calculateSize() int64 {
	primary, fallback := s.roots.Roots()
	seen := make(map[string]struct{}, 2)
	var total int64
	for _, root := range []string{primary, fallback} {
		if root == "" {
			continue
		}
		key := strings.ToLower(filepath.Clean(root))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		_ = walkSize(root, &total)
	}
	return total
}

func cacheFileSize(root, hash string) int64 {
	var total int64
	for _, path := range []string{objectPath(root, hash), metadataPath(root, hash)} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

func (s *DiskStore) Inspect(ctx context.Context) (HealthReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	primary, fallback := s.roots.Roots()
	report := HealthReport{CheckedAt: time.Now().UTC()}
	seenRoots := make(map[string]struct{})
	for _, root := range []string{primary, fallback} {
		if root == "" {
			continue
		}
		if _, seen := seenRoots[root]; seen {
			continue
		}
		seenRoots[root] = struct{}{}
		report.Roots = append(report.Roots, root)
		metadataRoot := filepath.Join(root, "metadata")
		err := filepath.WalkDir(metadataRoot, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				return nil
			}
			hash := strings.TrimSuffix(entry.Name(), ".json")
			if !validHash(hash) {
				return nil
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			report.Files++
			entryValue, body, err := readObject(root, hash)
			if err != nil {
				report.InvalidFiles++
				report.InvalidBytes += info.Size()
				return nil
			}
			report.ValidFiles++
			report.Bytes += int64(len(body))
			_ = entryValue
			return nil
		})
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func readObject(root, hash string) (CacheEntry, []byte, error) {
	objectPath := objectPath(root, hash)
	metadataPath := metadataPath(root, hash)
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return CacheEntry{}, nil, err
	}
	var entry CacheEntry
	if err := entry.Unmarshal(metadataBytes); err != nil {
		return CacheEntry{}, nil, fmt.Errorf("decode metadata: %w", err)
	}
	if entry.Version != MetadataVersion || entry.ContentLength < 0 {
		return CacheEntry{}, nil, errors.New("invalid cache metadata")
	}
	body, err := os.ReadFile(objectPath)
	if err != nil {
		return CacheEntry{}, nil, err
	}
	if int64(len(body)) != entry.ContentLength {
		return CacheEntry{}, nil, errors.New("cache content length mismatch")
	}
	if entry.SHA256 != "" {
		digest := sha256.Sum256(body)
		if !strings.EqualFold(entry.SHA256, hex.EncodeToString(digest[:])) {
			return CacheEntry{}, nil, errors.New("cache checksum mismatch")
		}
	}
	entry.LastAccessed = time.Now()
	return entry, body, nil
}

func writeObject(root, hash string, entry CacheEntry, body []byte) error {
	if int64(len(body)) == 0 {
		return errors.New("refusing to cache an empty object")
	}
	if err := os.MkdirAll(filepath.Dir(objectPath(root, hash)), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(metadataPath(root, hash)), 0o700); err != nil {
		return err
	}
	entry.Version = MetadataVersion
	entry.ContentLength = int64(len(body))
	digest := sha256.Sum256(body)
	entry.SHA256 = hex.EncodeToString(digest[:])
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	entry.LastAccessed = time.Now()

	objectTemp, err := os.CreateTemp(filepath.Dir(objectPath(root, hash)), ".object-*.tmp")
	if err != nil {
		return err
	}
	objectTempName := objectTemp.Name()
	defer os.Remove(objectTempName)
	if _, err := objectTemp.Write(body); err != nil {
		_ = objectTemp.Close()
		return err
	}
	if err := objectTemp.Sync(); err != nil {
		_ = objectTemp.Close()
		return err
	}
	if err := objectTemp.Close(); err != nil {
		return err
	}
	if err := replaceFile(objectTempName, objectPath(root, hash)); err != nil {
		return err
	}

	metadata, err := entry.Marshal()
	if err != nil {
		return err
	}
	metadata = append(metadata, '\n')
	metadataTemp, err := os.CreateTemp(filepath.Dir(metadataPath(root, hash)), ".metadata-*.tmp")
	if err != nil {
		return err
	}
	metadataTempName := metadataTemp.Name()
	defer os.Remove(metadataTempName)
	if err := metadataTemp.Chmod(0o600); err != nil {
		_ = metadataTemp.Close()
		return err
	}
	if _, err := metadataTemp.Write(metadata); err != nil {
		_ = metadataTemp.Close()
		return err
	}
	if err := metadataTemp.Sync(); err != nil {
		_ = metadataTemp.Close()
		return err
	}
	if err := metadataTemp.Close(); err != nil {
		return err
	}
	return replaceFile(metadataTempName, metadataPath(root, hash))
}

func replaceFile(tempName, destination string) error {
	if err := os.Rename(tempName, destination); err == nil {
		return nil
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tempName, destination)
}

func objectPath(root, hash string) string {
	return filepath.Join(root, "objects", hash[:2], hash)
}

func metadataPath(root, hash string) string {
	return filepath.Join(root, "metadata", hash[:2], hash+".json")
}

func validHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func copyFile(ctx context.Context, source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".copy-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	buffer := make([]byte, 128*1024)
	for {
		select {
		case <-ctx.Done():
			_ = tmp.Close()
			return ctx.Err()
		default:
		}
		count, readErr := in.Read(buffer)
		if count > 0 {
			if _, err := tmp.Write(buffer[:count]); err != nil {
				_ = tmp.Close()
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = tmp.Close()
			return readErr
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmpName, destination)
}
