package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	Context        context.Context
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
	lifecycle     context.Context
	writeSlots    chan struct{}
	writeGate     *writeGate
}

// writeGate prevents a cache-root transition from racing with a disk write.
// Writers enter as readers for the short duration of their disk operation;
// a transition blocks new readers and waits for the existing ones to leave.
type writeGate struct {
	mu      sync.Mutex
	active  int
	blocked bool
	changed chan struct{}
}

func newWriteGate() *writeGate {
	return &writeGate{changed: make(chan struct{})}
}

func (g *writeGate) enter(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		g.mu.Lock()
		if !g.blocked {
			g.active++
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *writeGate) leave() {
	g.mu.Lock()
	g.active--
	if g.active == 0 {
		close(g.changed)
		g.changed = make(chan struct{})
	}
	g.mu.Unlock()
}

func (g *writeGate) begin(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.blocked {
		g.mu.Unlock()
		return errors.New("cache write barrier is already active")
	}
	g.blocked = true
	g.mu.Unlock()

	for {
		g.mu.Lock()
		if g.active == 0 {
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			g.end()
			return ctx.Err()
		}
	}
}

func (g *writeGate) end() {
	g.mu.Lock()
	if g.blocked {
		g.blocked = false
		close(g.changed)
		g.changed = make(chan struct{})
	}
	g.mu.Unlock()
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
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	return &Manager{
		disk:          NewDiskStore(cfg.Roots),
		ram:           NewRAMCache(cfg.RAMBytes, cfg.RAMObjectBytes),
		maxObjectSize: cfg.MaxObjectBytes,
		origin:        cfg.Origin,
		stats:         cfg.Stats,
		logs:          cfg.Logs,
		lifecycle:     cfg.Context,
		writeSlots:    make(chan struct{}, 4),
		writeGate:     newWriteGate(),
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
		served := servedBytes(request, entry, body)
		m.stats.AddCacheBytesServed(served)
		m.stats.AddBytesSaved(served)
		m.log(logging.CategoryHit, request, "RAM", len(body))
		cached := Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceRAM, Cacheable: true}
		if request.Method == http.MethodGet && !IsFresh(entry, time.Now()) {
			return m.revalidateCached(ctx, request, hash, canonical, cached)
		}
		return cached, nil
	}
	if entry, body, root, diskErr := m.disk.Read(hash); diskErr == nil {
		m.stats.DiskHit()
		served := servedBytes(request, entry, body)
		m.stats.AddCacheBytesServed(served)
		m.stats.AddBytesSaved(served)
		m.ram.Put(hash, entry, body)
		primary, _ := m.disk.Roots().Roots()
		if root != primary {
			m.promote(hash, entry, body)
		}
		m.log(logging.CategoryHit, request, "Disk", len(body))
		cached := Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceDisk, Cacheable: true}
		if request.Method == http.MethodGet && !IsFresh(entry, time.Now()) {
			return m.revalidateCached(ctx, request, hash, canonical, cached)
		}
		return cached, nil
	}
	if request.Method == http.MethodHead {
		// HEAD never creates a cache entry and does not participate in the
		// GET singleflight. A concurrent GET must perform its own body fetch.
		return m.fetchOrigin(m.lifecycle, request, hash, canonical)
	}

	resultChannel := m.flight.DoChan(hash, func() (any, error) {
		// Another request may have populated RAM while this goroutine waited
		// to enter the singleflight function.
		if entry, body, ok := m.ram.Get(hash); ok {
			return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceRAM, Cacheable: true}, nil
		}
		if entry, body, _, diskErr := m.disk.Read(hash); diskErr == nil {
			m.ram.Put(hash, entry, body)
			return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceDisk, Cacheable: true}, nil
		}
		return m.fetchOrigin(m.lifecycle, request, hash, canonical)
	})
	var resultValue singleflight.Result
	select {
	case resultValue = <-resultChannel:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	if resultValue.Err != nil {
		return Result{}, resultValue.Err
	}
	result, ok := resultValue.Val.(Result)
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
	response, err := m.origin.Do(ctx, m.originRequest(ctx, request, nil))
	if err != nil {
		m.stats.Error()
		return Result{}, err
	}
	return m.consumeOriginResponse(request, hash, canonical, response)
}

func (m *Manager) revalidateCached(ctx context.Context, request *http.Request, hash, canonical string, cached Result) (Result, error) {
	resultChannel := m.flight.DoChan("revalidate:"+hash, func() (any, error) {
		return m.fetchRevalidated(m.lifecycle, request, hash, canonical, cached)
	})
	var resultValue singleflight.Result
	select {
	case resultValue = <-resultChannel:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	if resultValue.Err != nil {
		if m.lifecycle.Err() == nil {
			m.log(logging.CategoryError, request, "revalidation failed; serving stale cache: "+resultValue.Err.Error(), len(cached.Body))
			return cached, nil
		}
		return Result{}, resultValue.Err
	}
	result, ok := resultValue.Val.(Result)
	if !ok {
		return Result{}, errors.New("invalid revalidation result")
	}
	return result, nil
}

func (m *Manager) fetchRevalidated(ctx context.Context, request *http.Request, hash, canonical string, cached Result) (Result, error) {
	response, err := m.origin.Do(ctx, m.originRequest(ctx, request, &cached.Entry))
	if err != nil {
		m.stats.Error()
		return Result{}, err
	}
	if response != nil && response.StatusCode == http.StatusNotModified {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		entry := cached.Entry.Clone()
		if entry.Headers == nil {
			entry.Headers = make(http.Header)
		}
		for key, values := range SanitizeHeaders(response.Header) {
			if strings.EqualFold(key, "Content-Length") {
				continue
			}
			entry.Headers.Del(key)
			for _, value := range values {
				entry.Headers.Add(key, value)
			}
		}
		entry.ETag = entry.Headers.Get("ETag")
		entry.LastModified = entry.Headers.Get("Last-Modified")
		entry.CreatedAt = time.Now()
		entry.LastAccessed = entry.CreatedAt
		m.store(hash, entry, cached.Body, request)
		m.log(logging.CategoryHit, request, "revalidated 304", len(cached.Body))
		return Result{Key: hash, Canonical: canonical, Entry: entry, Body: cached.Body, Source: cached.Source, Cacheable: true}, nil
	}
	return m.consumeOriginResponse(request, hash, canonical, response)
}

func (m *Manager) originRequest(ctx context.Context, request *http.Request, cached *CacheEntry) *http.Request {
	originRequest := request.Clone(ctx)
	originRequest.Method = request.Method
	originRequest.RequestURI = ""
	originRequest.Header = request.Header.Clone()
	for _, key := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Match", "Cookie", "Authorization", "Proxy-Authorization"} {
		originRequest.Header.Del(key)
	}
	// Cache bytes are stored and served byte-for-byte. Asking for identity
	// avoids treating a compressed representation as a decoded asset.
	originRequest.Header.Set("Accept-Encoding", "identity")
	if cached != nil {
		if cached.ETag != "" {
			originRequest.Header.Set("If-None-Match", cached.ETag)
		} else if cached.LastModified != "" {
			originRequest.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}
	return originRequest
}

func (m *Manager) consumeOriginResponse(request *http.Request, hash, canonical string, response *http.Response) (Result, error) {
	if response == nil {
		m.stats.Error()
		return Result{}, errors.New("origin returned an empty response")
	}
	if request.Method == http.MethodHead {
		if response.Body != nil {
			defer response.Body.Close()
		}
		entry := newEntry(request.URL, response, nil)
		m.log(logging.CategoryMiss, request, "origin", 0)
		return Result{Key: hash, Canonical: canonical, Entry: entry, Source: SourceOrigin, Cacheable: false}, nil
	}
	if response.Body == nil {
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
	m.store(hash, entry, body, request)
	m.log(logging.CategoryMiss, request, "origin", len(body))
	return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceOrigin, Cacheable: true}, nil
}

func (m *Manager) store(hash string, entry CacheEntry, body []byte, request *http.Request) {
	m.ram.Put(hash, entry, body)
	if err := m.writeGate.enter(m.lifecycle); err != nil {
		return
	}
	m.writes.Add(1)
	go func() {
		defer m.writes.Done()
		defer m.writeGate.leave()
		select {
		case m.writeSlots <- struct{}{}:
			defer func() { <-m.writeSlots }()
		case <-m.lifecycle.Done():
			return
		}
		if err := m.disk.Put(hash, entry, body); err != nil {
			m.stats.Error()
			m.log(logging.CategoryError, request, "disk store failed: "+err.Error(), len(body))
			return
		}
		m.log(logging.CategoryStore, request, "stored", len(body))
	}()
}

func (m *Manager) promote(hash string, entry CacheEntry, body []byte) {
	if err := m.writeGate.enter(m.lifecycle); err != nil {
		return
	}
	defer m.writeGate.leave()
	if err := m.disk.Put(hash, entry, body); err != nil {
		m.stats.Error()
		m.log(logging.CategoryError, nil, "cache promotion failed: "+err.Error(), len(body))
	}
}

// BeginRootTransition blocks new disk writes, drains existing writes, and
// returns a release function. The caller must hold the barrier while it
// switches RootSet or renames the cache root.
func (m *Manager) BeginRootTransition(ctx context.Context) (func(), error) {
	if err := m.writeGate.begin(ctx); err != nil {
		return nil, err
	}
	if err := m.disk.Wait(ctx); err != nil {
		m.writeGate.end()
		return nil, err
	}
	return m.writeGate.end, nil
}

func (m *Manager) Clear(ctx context.Context) error {
	release, err := m.BeginRootTransition(ctx)
	if err != nil {
		return err
	}
	defer release()
	m.ram.Clear()
	return m.disk.Clear()
}

func (m *Manager) Inspect(ctx context.Context) (HealthReport, error) {
	release, err := m.BeginRootTransition(ctx)
	if err != nil {
		return HealthReport{}, err
	}
	defer release()
	return m.disk.Inspect(ctx)
}

func (m *Manager) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := m.BeginRootTransition(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := m.disk.Wait(ctx); err != nil {
		return err
	}
	m.writes.Wait()
	return nil
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
	result := make(http.Header, len(headers))
	for key, values := range headers {
		canonicalKey := http.CanonicalHeaderKey(key)
		if canonicalKey == "" {
			continue
		}
		result[canonicalKey] = append(result[canonicalKey], values...)
	}
	for _, key := range []string{"Cookie", "Authorization", "Proxy-Authorization", "Set-Cookie", "Set-Cookie2"} {
		result.Del(key)
	}
	return result
}

func servedBytes(request *http.Request, entry CacheEntry, body []byte) int64 {
	if request == nil || request.Method == http.MethodHead || entry.StatusCode == http.StatusNotModified {
		return 0
	}
	if entry.StatusCode != http.StatusOK || len(body) == 0 {
		return int64(len(body))
	}
	if value := request.Header.Get("Range"); value != "" && ifRangeMatches(request, entry) {
		byteRange, err := ParseRange(value, int64(len(body)))
		if err != nil {
			return 0
		}
		return byteRange.Length()
	}
	return int64(len(body))
}

func IsStaticContentType(value string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	return strings.HasPrefix(mediaType, "image/") ||
		strings.HasPrefix(mediaType, "audio/") ||
		strings.HasPrefix(mediaType, "video/") ||
		strings.HasPrefix(mediaType, "font/") ||
		strings.Contains(mediaType, "javascript") ||
		strings.Contains(mediaType, "css") ||
		strings.Contains(mediaType, "json")
}

// IsFresh applies explicit HTTP freshness directives when a CDN provides
// them. Resources without a freshness directive remain fresh indefinitely;
// this is intentional for GBF's versioned static assets and avoids adding a
// surprising network request to every normal cache hit.
func IsFresh(entry CacheEntry, now time.Time) bool {
	cacheControl := strings.ToLower(entry.Headers.Get("Cache-Control"))
	for _, directive := range strings.Split(cacheControl, ",") {
		parts := strings.SplitN(strings.TrimSpace(directive), "=", 2)
		name := strings.Trim(strings.TrimSpace(parts[0]), `"`)
		switch name {
		case "no-store", "no-cache", "must-revalidate", "proxy-revalidate":
			return false
		case "max-age", "s-maxage":
			if len(parts) != 2 {
				return false
			}
			seconds, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(parts[1]), `"`), 10, 64)
			if err != nil || seconds < 0 || entry.CreatedAt.IsZero() {
				return false
			}
			return now.Before(entry.CreatedAt.Add(time.Duration(seconds) * time.Second))
		}
	}
	if expires := entry.Headers.Get("Expires"); expires != "" {
		expiresAt, err := http.ParseTime(expires)
		if err == nil {
			return now.Before(expiresAt)
		}
	}
	return true
}

func (m *Manager) DiskSize() int64 {
	return m.disk.DiskSize()
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
