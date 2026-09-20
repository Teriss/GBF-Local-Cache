package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"

	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/stats"
)

type OriginClient interface {
	Do(context.Context, *http.Request) (*http.Response, error)
}

type Source string

const (
	SourceRAM    Source = "ram"
	SourceDisk   Source = "disk"
	SourceOrigin Source = "origin"
)

type Result struct {
	Key       string
	Canonical string
	Entry     CacheEntry
	Body      []byte
	Source    Source
	Cacheable bool
}

type ManagerConfig struct {
	Root           string
	RAMBytes       int64
	RAMObjectBytes int64
	MaxObjectBytes int64
	Origin         OriginClient
	Stats          *stats.Stats
	Logs           *logging.Ring
	Roots          *RootSet
}

type Manager struct {
	disk          *DiskStore
	ram           *RAMCache
	maxObjectSize int64
	origin        OriginClient
	stats         *stats.Stats
	logs          *logging.Ring
	flight        singleflight.Group
	writes        sync.WaitGroup
}

func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Origin == nil {
		return nil, errors.New("cache origin client is required")
	}
	if cfg.Roots == nil {
		if cfg.Root == "" {
			return nil, errors.New("cache root is required")
		}
		cfg.Roots = NewRootSet(cfg.Root)
	}
	if cfg.RAMBytes <= 0 {
		cfg.RAMBytes = 256 << 20
	}
	if cfg.RAMObjectBytes <= 0 {
		cfg.RAMObjectBytes = 8 << 20
	}
	if cfg.MaxObjectBytes <= 0 {
		cfg.MaxObjectBytes = 512 << 20
	}
	if cfg.Stats == nil {
		cfg.Stats = &stats.Stats{}
	}
	if cfg.Logs == nil {
		cfg.Logs = logging.NewRing(5000)
	}
	return &Manager{
		disk:          NewDiskStore(cfg.Roots),
		ram:           NewRAMCache(cfg.RAMBytes, cfg.RAMObjectBytes),
		maxObjectSize: cfg.MaxObjectBytes,
		origin:        cfg.Origin,
		stats:         cfg.Stats,
		logs:          cfg.Logs,
	}, nil
}

func (m *Manager) Roots() *RootSet {
	return m.disk.Roots()
}

func (m *Manager) RAMUsedBytes() int64 {
	return m.ram.UsedBytes()
}

func (m *Manager) RAMMaxBytes() int64 {
	return m.ram.MaxBytes()
}

func (m *Manager) Fetch(ctx context.Context, request *http.Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request == nil || request.URL == nil {
		return Result{}, errors.New("cache request is missing")
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		return Result{}, fmt.Errorf("method %s is not cacheable", request.Method)
	}
	canonical, hash, err := CanonicalKey(request.URL)
	if err != nil {
		return Result{}, err
	}
	if entry, body, ok := m.ram.Get(hash); ok {
		m.stats.RamHit()
		m.stats.AddCacheBytesServed(int64(len(body)))
		m.stats.AddBytesSaved(int64(len(body)))
		m.log(logging.CategoryHit, request, "RAM", len(body))
		return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceRAM, Cacheable: true}, nil
	}
	if entry, body, root, diskErr := m.disk.Read(hash); diskErr == nil {
		m.stats.DiskHit()
		m.stats.AddCacheBytesServed(int64(len(body)))
		m.stats.AddBytesSaved(int64(len(body)))
		m.ram.Put(hash, entry, body)
		primary, _ := m.disk.Roots().Roots()
		if root != primary {
			m.disk.Promote(hash, entry, body)
		}
		m.log(logging.CategoryHit, request, "Disk", len(body))
		return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceDisk, Cacheable: true}, nil
	}

	value, err, _ := m.flight.Do(hash, func() (any, error) {
		// Another request may have populated RAM while this goroutine waited
		// to enter the singleflight function.
		if entry, body, ok := m.ram.Get(hash); ok {
			return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceRAM, Cacheable: true}, nil
		}
		if entry, body, _, diskErr := m.disk.Read(hash); diskErr == nil {
			m.ram.Put(hash, entry, body)
			return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceDisk, Cacheable: true}, nil
		}
		return m.fetchOrigin(ctx, request, hash, canonical)
	})
	if err != nil {
		return Result{}, err
	}
	result, ok := value.(Result)
	if !ok {
		return Result{}, errors.New("invalid singleflight result")
	}
	if result.Source == "" {
		result.Source = SourceOrigin
	}
	return result, nil
}

func (m *Manager) fetchOrigin(ctx context.Context, request *http.Request, hash, canonical string) (Result, error) {
	m.stats.Miss()
	originRequest := request.Clone(ctx)
	originRequest.Method = http.MethodGet
	originRequest.RequestURI = ""
	originRequest.Header = request.Header.Clone()
	for _, key := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Match", "Cookie", "Authorization"} {
		originRequest.Header.Del(key)
	}
	// Cache bytes are stored and served byte-for-byte. Asking for identity
	// avoids treating a compressed representation as a decoded asset.
	originRequest.Header.Set("Accept-Encoding", "identity")
	response, err := m.origin.Do(ctx, originRequest)
	if err != nil {
		m.stats.Error()
		return Result{}, err
	}
	if response == nil || response.Body == nil {
		m.stats.Error()
		return Result{}, errors.New("origin returned an empty response")
	}
	defer response.Body.Close()
	if response.ContentLength > m.maxObjectSize {
		m.stats.Error()
		return Result{}, fmt.Errorf("origin object is larger than %d bytes", m.maxObjectSize)
	}
	limited := io.LimitReader(response.Body, m.maxObjectSize+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		m.stats.Error()
		return Result{}, readErr
	}
	m.stats.AddOriginBytes(int64(len(body)))
	entry := newEntry(request.URL, response, body)
	if response.StatusCode != http.StatusOK {
		return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceOrigin, Cacheable: false}, nil
	}
	if int64(len(body)) > m.maxObjectSize {
		m.stats.Error()
		return Result{}, fmt.Errorf("origin object is larger than configured maximum")
	}
	if err := validateResponse(response, body); err != nil {
		m.stats.Error()
		m.log(logging.CategoryError, request, err.Error(), len(body))
		return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceOrigin, Cacheable: false}, nil
	}
	entry.Headers.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	entry.ContentLength = int64(len(body))
	m.ram.Put(hash, entry, body)
	m.writes.Add(1)
	go func() {
		defer m.writes.Done()
		if err := m.disk.Put(hash, entry, body); err != nil {
			m.stats.Error()
			m.log(logging.CategoryError, request, "disk store failed: "+err.Error(), len(body))
			return
		}
		m.log(logging.CategoryStore, request, "stored", len(body))
	}()
	m.log(logging.CategoryMiss, request, "origin", len(body))
	return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceOrigin, Cacheable: true}, nil
}

func (m *Manager) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		m.writes.Wait()
		_ = m.disk.Wait(context.Background())
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Clear(ctx context.Context) error {
	if err := m.Wait(ctx); err != nil {
		return err
	}
	m.ram.Clear()
	return m.disk.Clear()
}

func (m *Manager) Inspect(ctx context.Context) (HealthReport, error) {
	if err := m.Wait(ctx); err != nil {
		return HealthReport{}, err
	}
	return m.disk.Inspect(ctx)
}

func (m *Manager) log(category logging.Category, request *http.Request, message string, bytes int) {
	target := ""
	method := ""
	if request != nil && request.URL != nil {
		target = request.URL.Host + request.URL.EscapedPath()
		if target == "" {
			target = "/"
		}
		method = request.Method
	}
	m.logs.Add(logging.Entry{Category: category, Method: method, Target: target, Message: message, Duration: fmt.Sprintf("%d bytes", bytes)})
}

func SanitizeHeaders(headers http.Header) http.Header {
	result := headers.Clone()
	for _, key := range []string{"Cookie", "Authorization", "Proxy-Authorization", "Set-Cookie"} {
		result.Del(key)
	}
	return result
}

func IsStaticContentType(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, "image/") || strings.HasPrefix(value, "audio/") || strings.HasPrefix(value, "video/") || strings.HasPrefix(value, "font/") || strings.Contains(value, "javascript") || strings.Contains(value, "css") || strings.Contains(value, "json")
}

func (m *Manager) DiskSize() int64 {
	primary, fallback := m.disk.Roots().Roots()
	var total int64
	for _, root := range []string{primary, fallback} {
		if root == "" {
			continue
		}
		_ = walkSize(root, &total)
	}
	return total
}

func walkSize(root string, total *int64) error {
	return filepath.WalkDir(root, func(path string, entryDir fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// The state directory contains migration bookkeeping rather than
		// cached objects. In particular, migration.json must not inflate the
		// cache size shown in the UI.
		if entryDir.IsDir() && relative != "." && strings.EqualFold(filepath.ToSlash(relative), "state") {
			return filepath.SkipDir
		}
		if entryDir.IsDir() {
			return nil
		}
		info, err := entryDir.Info()
		if err != nil {
			return err
		}
		*total += info.Size()
		return nil
	})
}
