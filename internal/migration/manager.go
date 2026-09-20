package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/platform"
)

// State describes the lifecycle of an online cache migration.
type State string

const (
	StateIdle      State = "idle"
	StateRunning   State = "running"
	StatePaused    State = "paused"
	StateCompleted State = "completed"
	StateCanceled  State = "canceled"
	StateError     State = "error"
)

// Status is deliberately made of plain JSON values so it can be exposed by
// the Wails service snapshot without leaking migration internals to the UI.
type Status struct {
	State               State     `json:"state"`
	OldRoot             string    `json:"old_root,omitempty"`
	NewRoot             string    `json:"new_root,omitempty"`
	TotalBytes          int64     `json:"total_bytes"`
	CopiedBytes         int64     `json:"copied_bytes"`
	TotalFiles          int64     `json:"total_files"`
	CopiedFiles         int64     `json:"copied_files"`
	SpeedBytesPerSecond int64     `json:"speed_bytes_per_second"`
	CurrentFile         string    `json:"current_file,omitempty"`
	Error               string    `json:"error,omitempty"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	FinishedAt          time.Time `json:"finished_at,omitempty"`
}

type fileRecord struct {
	Relative string
	Size     int64
}

type persistedState struct {
	Status  Status    `json:"status"`
	SavedAt time.Time `json:"saved_at"`
}

// Manager copies cache objects while the cache RootSet is already configured
// as New(primary) + Old(fallback). New writes therefore continue during the
// copy, and a hit from the old root can be lazily promoted by cache.Manager.
type Manager struct {
	mu          sync.RWMutex
	roots       *cache.RootSet
	logs        *logging.Ring
	status      Status
	cancel      context.CancelFunc
	done        chan struct{}
	paused      bool
	wake        chan struct{}
	callback    func(Status)
	persistMu   sync.Mutex
	lastPersist time.Time
}

func New(roots *cache.RootSet, logs *logging.Ring) *Manager {
	if logs == nil {
		logs = logging.NewRing(5000)
	}
	return &Manager{roots: roots, logs: logs, status: Status{State: StateIdle}}
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) Roots() *cache.RootSet {
	return m.roots
}

// Start validates and begins an online migration. The root switch happens
// before the scan so the old root becomes an immutable source. Callers that
// have a live cache manager should hold its write barrier while invoking this
// method, which closes the scan/switch race for in-flight disk writes.
func (m *Manager) Start(parent context.Context, oldRoot, newRoot string, callback func(Status)) error {
	return m.StartWithWorker(parent, parent, oldRoot, newRoot, callback)
}

// StartWithWorker starts a migration using scanContext for the synchronous
// validation/scan and workerContext for the background copy. Keeping these
// contexts separate prevents a short-lived RPC timeout from canceling a
// migration after the RPC has already returned successfully.
func (m *Manager) StartWithWorker(scanContext, workerContext context.Context, oldRoot, newRoot string, callback func(Status)) error {
	if scanContext == nil {
		scanContext = context.Background()
	}
	if workerContext == nil {
		workerContext = context.Background()
	}
	oldRoot, err := absoluteClean(oldRoot)
	if err != nil {
		return fmt.Errorf("old cache root: %w", err)
	}
	newRoot, err = absoluteClean(newRoot)
	if err != nil {
		return fmt.Errorf("new cache root: %w", err)
	}
	if samePath(oldRoot, newRoot) {
		return errors.New("new cache root is the same as the current root")
	}
	if pathContains(oldRoot, newRoot) || pathContains(newRoot, oldRoot) {
		return errors.New("cache roots cannot contain one another")
	}
	if m.roots == nil {
		return errors.New("migration root set is not initialized")
	}
	if err := cache.EnsureRoot(oldRoot); err != nil {
		return fmt.Errorf("old cache root is not valid: %w", err)
	}
	if err := cache.EnsureRoot(newRoot); err != nil {
		return fmt.Errorf("new cache root is not writable: %w", err)
	}

	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return errors.New("cache migration is already running")
	}
	m.mu.Unlock()

	// New writes must target the destination before the source scan begins.
	// If scanning fails, restore the old primary root before returning.
	m.roots.BeginMigration(newRoot, oldRoot)
	files, totalBytes, err := scanFiles(scanContext, oldRoot)
	if err != nil {
		m.roots.RestoreFallback()
		return fmt.Errorf("scan old cache root: %w", err)
	}
	ctx, cancel := context.WithCancel(workerContext)
	m.mu.Lock()
	m.cancel = cancel
	m.done = make(chan struct{})
	m.paused = false
	m.wake = make(chan struct{})
	m.callback = callback
	m.status = Status{
		State:      StateRunning,
		OldRoot:    oldRoot,
		NewRoot:    newRoot,
		TotalBytes: totalBytes,
		TotalFiles: int64(len(files)),
		StartedAt:  time.Now().UTC(),
	}
	done := m.done
	m.mu.Unlock()

	// Reads still fall back to the source until FinishMigration is called.
	m.log(logging.CategoryMigration, "started: "+oldRoot+" -> "+newRoot)
	m.persist(true)
	go m.run(ctx, done, files)
	return nil
}

// CompletedRoot reports a migration that finished before the process could
// persist the new configured root. It is intentionally conservative: only a
// completed state whose old root matches the configured root and whose new
// root still exists is accepted.
func CompletedRoot(configuredRoot string) (string, bool, error) {
	configuredRoot, err := absoluteClean(configuredRoot)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(filepath.Join(configuredRoot, "state", "migration.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var saved persistedState
	if err := json.Unmarshal(data, &saved); err != nil {
		return "", false, fmt.Errorf("decode completed migration state: %w", err)
	}
	if saved.Status.State != StateCompleted {
		return "", false, nil
	}
	oldRoot, err := absoluteClean(saved.Status.OldRoot)
	if err != nil || !samePath(oldRoot, configuredRoot) {
		return "", false, nil
	}
	newRoot, err := absoluteClean(saved.Status.NewRoot)
	if err != nil || samePath(newRoot, configuredRoot) {
		return "", false, nil
	}
	info, err := os.Stat(newRoot)
	if err != nil {
		return "", false, nil
	}
	if !info.IsDir() {
		return "", false, nil
	}
	return newRoot, true, nil
}

// Recover resumes a migration whose process was interrupted. The old root is
// still the configured root after an interrupted run, so it remains safe to
// serve from it while the destination is rebuilt and verified.
func (m *Manager) Recover(parent context.Context, configuredOldRoot string, callback func(Status)) (bool, error) {
	return m.RecoverWithWorker(parent, parent, configuredOldRoot, callback)
}

// RecoverWithWorker is the restart counterpart of StartWithWorker.
func (m *Manager) RecoverWithWorker(scanContext, workerContext context.Context, configuredOldRoot string, callback func(Status)) (bool, error) {
	if scanContext == nil {
		scanContext = context.Background()
	}
	if workerContext == nil {
		workerContext = context.Background()
	}
	configuredOldRoot, err := absoluteClean(configuredOldRoot)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(configuredOldRoot, "state", "migration.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var saved persistedState
	if err := json.Unmarshal(data, &saved); err != nil {
		return false, fmt.Errorf("decode migration state: %w", err)
	}
	if saved.Status.State != StateRunning && saved.Status.State != StatePaused {
		return false, nil
	}
	if saved.Status.OldRoot == "" || saved.Status.NewRoot == "" {
		return false, errors.New("migration state has incomplete roots")
	}
	if !samePath(saved.Status.OldRoot, configuredOldRoot) {
		return false, errors.New("migration state does not belong to the configured cache root")
	}
	if err := m.StartWithWorker(scanContext, workerContext, saved.Status.OldRoot, saved.Status.NewRoot, callback); err != nil {
		return false, err
	}
	m.log(logging.CategoryMigration, "resumed migration after process restart")
	return true, nil
}

func (m *Manager) run(ctx context.Context, done chan struct{}, files []fileRecord) {
	defer close(done)
	started := time.Now()
	for _, file := range files {
		if err := m.waitIfPaused(ctx); err != nil {
			m.finishCanceled(err)
			return
		}
		m.setCurrent(file.Relative)
		source := filepath.Join(m.statusRoots().old, file.Relative)
		destination := filepath.Join(m.statusRoots().new, file.Relative)
		if err := copyAndVerify(ctx, source, destination, file.Size); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				m.finishCanceled(err)
			} else {
				m.finishError(err)
			}
			return
		}
		m.mu.Lock()
		m.status.CopiedFiles++
		m.status.CopiedBytes += file.Size
		elapsed := time.Since(started)
		if elapsed > 0 {
			m.status.SpeedBytesPerSecond = int64(float64(m.status.CopiedBytes) / elapsed.Seconds())
		}
		m.mu.Unlock()
		m.persist(false)
	}

	if err := verifyMigration(m.statusRoots().new, files); err != nil {
		m.finishError(err)
		return
	}
	m.roots.FinishMigration()
	m.mu.Lock()
	m.status.State = StateCompleted
	m.status.CurrentFile = ""
	m.status.FinishedAt = time.Now().UTC()
	m.status.Error = ""
	callback := m.callback
	m.cancel = nil
	m.callback = nil
	m.mu.Unlock()
	m.persist(true)
	m.log(logging.CategoryMigration, "completed")
	if callback != nil {
		callback(m.Status())
	}
}

func (m *Manager) Pause() error {
	m.mu.Lock()
	if m.cancel == nil || m.status.State != StateRunning {
		m.mu.Unlock()
		return errors.New("cache migration is not running")
	}
	m.paused = true
	m.status.State = StatePaused
	m.mu.Unlock()
	m.persist(true)
	return nil
}

func (m *Manager) Resume() error {
	m.mu.Lock()
	if m.cancel == nil || m.status.State != StatePaused {
		m.mu.Unlock()
		return errors.New("cache migration is not paused")
	}
	m.paused = false
	m.status.State = StateRunning
	close(m.wake)
	m.wake = make(chan struct{})
	m.mu.Unlock()
	m.persist(true)
	return nil
}

func (m *Manager) Cancel() error {
	m.mu.RLock()
	cancel := m.cancel
	m.mu.RUnlock()
	if cancel == nil {
		return errors.New("cache migration is not running")
	}
	cancel()
	return nil
}

func (m *Manager) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	done := m.done
	m.mu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Close(ctx context.Context) error {
	_ = m.Cancel()
	return m.Wait(ctx)
}

func (m *Manager) waitIfPaused(ctx context.Context) error {
	for {
		m.mu.RLock()
		paused, wake := m.paused, m.wake
		m.mu.RUnlock()
		if !paused {
			return nil
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) setCurrent(relative string) {
	m.mu.Lock()
	m.status.CurrentFile = relative
	m.mu.Unlock()
}

type rootsSnapshot struct {
	old string
	new string
}

func (m *Manager) statusRoots() rootsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return rootsSnapshot{old: m.status.OldRoot, new: m.status.NewRoot}
}

func (m *Manager) finishCanceled(err error) {
	m.roots.RestoreFallback()
	m.finish(StateCanceled, err)
}

func (m *Manager) finishError(err error) {
	m.roots.RestoreFallback()
	m.finish(StateError, err)
}

func (m *Manager) finish(state State, err error) {
	m.mu.Lock()
	m.status.State = state
	m.status.CurrentFile = ""
	m.status.FinishedAt = time.Now().UTC()
	if err != nil && !errors.Is(err, context.Canceled) {
		m.status.Error = err.Error()
	}
	callback := m.callback
	m.cancel = nil
	m.callback = nil
	m.mu.Unlock()
	m.persist(true)
	m.log(logging.CategoryMigration, fmt.Sprintf("%s: %v", state, err))
	if callback != nil {
		callback(m.Status())
	}
}

func (m *Manager) persist(force bool) {
	status := m.Status()
	if status.NewRoot == "" {
		return
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	if !force && !m.lastPersist.IsZero() && time.Since(m.lastPersist) < time.Second {
		return
	}
	payload, err := json.MarshalIndent(persistedState{Status: status, SavedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		m.log(logging.CategoryError, "persist migration state: "+err.Error())
		return
	}
	payload = append(payload, '\n')
	if err := atomicWrite(filepath.Join(status.NewRoot, "state", "migration.json"), payload, 0o600); err != nil {
		m.log(logging.CategoryError, "persist migration state: "+err.Error())
		return
	}
	if status.OldRoot != "" {
		if err := atomicWrite(filepath.Join(status.OldRoot, "state", "migration.json"), payload, 0o600); err != nil {
			m.log(logging.CategoryError, "persist migration state: "+err.Error())
			return
		}
	}
	m.lastPersist = time.Now()
}

func (m *Manager) log(category logging.Category, message string) {
	if m.logs != nil {
		m.logs.Add(logging.Entry{Category: category, Message: message})
	}
}

func scanFiles(ctx context.Context, root string) ([]fileRecord, int64, error) {
	var files []fileRecord
	var total int64
	for _, directory := range cache.ManagedDataDirectories() {
		base := filepath.Join(root, directory)
		info, err := os.Lstat(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if !info.IsDir() {
			return nil, 0, fmt.Errorf("managed cache path is not a directory: %s", directory)
		}
		err = filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink is not allowed in managed cache data: %s", relative)
			}
			fileInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if !fileInfo.Mode().IsRegular() {
				return fmt.Errorf("managed cache entry is not a regular file: %s", relative)
			}
			files = append(files, fileRecord{Relative: relative, Size: fileInfo.Size()})
			total += fileInfo.Size()
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return files, total, nil
}

func verifyMigration(root string, files []fileRecord) error {
	for _, file := range files {
		path := filepath.Join(root, file.Relative)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("verify %s: %w", file.Relative, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("verify %s: destination is not a regular file", file.Relative)
		}
		if strings.HasPrefix(filepath.ToSlash(file.Relative), "metadata/") && strings.HasSuffix(strings.ToLower(file.Relative), ".json") {
			if err := verifyMetadataPair(root, file.Relative); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyMetadataPair(root, relative string) error {
	metadataPath := filepath.Join(root, relative)
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("verify %s: read metadata: %w", relative, err)
	}
	var entry cache.CacheEntry
	if err := entry.Unmarshal(metadataBytes); err != nil {
		return fmt.Errorf("verify %s: decode metadata: %w", relative, err)
	}
	if entry.Version != cache.MetadataVersion || entry.ContentLength < 0 {
		return fmt.Errorf("verify %s: invalid cache metadata", relative)
	}
	metadataRoot := filepath.Join(root, "metadata")
	metadataRelative, err := filepath.Rel(metadataRoot, metadataPath)
	if err != nil || metadataRelative == "." || strings.HasPrefix(metadataRelative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("verify %s: invalid metadata path", relative)
	}
	hash := strings.TrimSuffix(metadataRelative, filepath.Ext(metadataRelative))
	objectPath := filepath.Join(root, "objects", hash)
	objectInfo, err := os.Stat(objectPath)
	if err != nil {
		return fmt.Errorf("verify %s: object is missing: %w", relative, err)
	}
	if !objectInfo.Mode().IsRegular() || objectInfo.Size() != entry.ContentLength {
		return fmt.Errorf("verify %s: metadata/object length mismatch", relative)
	}
	if entry.SHA256 != "" {
		if len(entry.SHA256) != 64 {
			return fmt.Errorf("verify %s: invalid SHA-256 metadata", relative)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("verify %s: invalid SHA-256 metadata: %w", relative, err)
		}
	}
	return nil
}

func copyAndVerify(ctx context.Context, source, destination string, expectedSize int64) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	// A cache write can legitimately create this path after the migration scan.
	// Never replace an existing destination: preserving the newer atomically
	// written value is safer than overwriting it with an older source value.
	if info, err := os.Stat(destination); err == nil {
		if info.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("migration destination is not a regular file: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".migration-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	digest := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(tmp, digest), in, make([]byte, 128*1024))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if written != expectedSize {
		_ = tmp.Close()
		return fmt.Errorf("copy size mismatch: got %d, want %d", written, expectedSize)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	installed, err := installIfAbsent(tmpName, destination)
	if err != nil {
		return err
	}
	if !installed {
		return nil
	}
	destinationDigest, err := hashFile(destination)
	if err != nil {
		return err
	}
	if destinationDigest != hex.EncodeToString(digest.Sum(nil)) {
		return errors.New("migration checksum mismatch")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func installIfAbsent(source, destination string) (bool, error) {
	// Linking a completed temporary file is atomic and, unlike rename, fails
	// when another writer has already created the destination.
	if err := os.Link(source, destination); err == nil {
		return true, os.Remove(source)
	} else if !errors.Is(err, os.ErrExist) && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(destination); err == nil {
		return false, os.Remove(source)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// Some filesystems do not support hard links. On those filesystems rename
	// is still safe on Windows (the target cannot be replaced by Rename), and
	// the existence check handles a concurrent creator.
	if err := os.Rename(source, destination); err == nil {
		return true, nil
	}
	if _, err := os.Stat(destination); err == nil {
		return false, os.Remove(source)
	}
	return false, fmt.Errorf("install migration file %s: %w", destination, os.ErrExist)
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func replaceFile(source, destination string) error {
	return platform.ReplaceFile(source, destination)
}

func absoluteClean(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil || relative == "." {
		return err == nil
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".migration-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmpName, path)
}
