package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/stats"
)

func TestManagerSingleFlightAndDiskRoundTrip(t *testing.T) {
	var origins atomic.Int64
	origin := originFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/javascript"}, "ETag": []string{`"v1"`}},
			Body:          io.NopCloser(strings.NewReader("console.log(1)")),
			ContentLength: 14,
			Request:       request,
		}, nil
	})
	root := t.TempDir()
	counters := &stats.Stats{}
	manager, err := NewManager(ManagerConfig{Root: root, Origin: origin, Stats: counters, Logs: logging.NewRing(20), RAMBytes: 1 << 20, RAMObjectBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/app.js?v=1")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header), Host: target.Host}
	const concurrency = 20
	results := make(chan Result, concurrency)
	errors := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			result, err := manager.Fetch(context.Background(), request)
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	for i := 0; i < concurrency; i++ {
		select {
		case err := <-errors:
			t.Fatal(err)
		case result := <-results:
			if string(result.Body) != "console.log(1)" {
				t.Fatalf("body = %q", result.Body)
			}
		}
	}
	if origins.Load() != 1 {
		t.Fatalf("origin requests = %d, want 1", origins.Load())
	}
	if counters.Snapshot().Misses != 1 {
		t.Fatalf("misses = %d, want 1", counters.Snapshot().Misses)
	}

	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.ram.Clear()
	result, err := manager.Fetch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != SourceDisk && result.Source != SourceRAM {
		t.Fatalf("second source = %q, want disk or ram", result.Source)
	}
	if origins.Load() != 1 {
		t.Fatalf("disk hit reached origin: %d", origins.Load())
	}
}

func TestManagerLeaderCancellationDoesNotCancelFollowers(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var origins atomic.Int64
	origin := originFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:          io.NopCloser(strings.NewReader("shared-body")),
			ContentLength: 11,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin, Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/shared.bin")
	firstRequest := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstRequest = firstRequest.WithContext(firstContext)
	firstErr := make(chan error, 1)
	go func() {
		_, err := manager.Fetch(firstContext, firstRequest)
		firstErr <- err
	}()
	<-started
	cancelFirst()
	select {
	case err := <-firstErr:
		if err == nil {
			t.Fatal("canceled leader unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not observe request cancellation")
	}
	secondResult := make(chan Result, 1)
	secondErr := make(chan error, 1)
	go func() {
		result, err := manager.Fetch(context.Background(), &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)})
		secondResult <- result
		secondErr <- err
	}()
	close(release)
	select {
	case err := <-secondErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("follower did not complete")
	}
	result := <-secondResult
	if string(result.Body) != "shared-body" {
		t.Fatalf("follower body = %q", result.Body)
	}
	if origins.Load() != 1 {
		t.Fatalf("origin requests = %d, want 1", origins.Load())
	}
}

func TestHeadMissDoesNotDownloadOrCacheBody(t *testing.T) {
	var method atomic.Value
	origin := originFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		method.Store(request.Method)
		if request.Method == http.MethodHead {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Length": []string{"123"}, "ETag": []string{`"head-v1"`}},
				Body:          http.NoBody,
				ContentLength: 123,
			}, nil
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: 4,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/head.bin")
	request := &http.Request{Method: http.MethodHead, URL: target, Header: make(http.Header)}
	result, err := manager.Fetch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cacheable || len(result.Body) != 0 || result.Entry.ContentLength != 123 {
		t.Fatalf("HEAD result = %#v", result)
	}
	if got := method.Load(); got != http.MethodHead {
		t.Fatalf("origin method = %v, want HEAD", got)
	}
	recorder := httptest.NewRecorder()
	WriteResult(recorder, request, result)
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") != "123" {
		t.Fatalf("HEAD response = status %d, body %q, content-length %q", recorder.Code, recorder.Body.String(), recorder.Header().Get("Content-Length"))
	}
	_, hash, err := CanonicalKey(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.disk.Read(hash); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HEAD created a cache entry: %v", err)
	}
}

func TestHeadAndGetMissesDoNotShareSingleFlight(t *testing.T) {
	headStarted := make(chan struct{})
	releaseHead := make(chan struct{})
	var origins atomic.Int64
	origin := originFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		if request.Method == http.MethodHead {
			close(headStarted)
			select {
			case <-releaseHead:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Length": []string{"4"}},
				Body:          http.NoBody,
				ContentLength: 4,
			}, nil
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: 4,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/shared.bin")
	headResult := make(chan error, 1)
	go func() {
		_, err := manager.Fetch(context.Background(), &http.Request{Method: http.MethodHead, URL: target, Header: make(http.Header)})
		headResult <- err
	}()
	<-headStarted

	getResult := make(chan Result, 1)
	getErr := make(chan error, 1)
	go func() {
		result, err := manager.Fetch(context.Background(), &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)})
		getResult <- result
		getErr <- err
	}()
	select {
	case err := <-getErr:
		if err != nil {
			t.Fatal(err)
		}
		result := <-getResult
		if string(result.Body) != "body" {
			t.Fatalf("GET body = %q, want body", result.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("GET waited for the in-flight HEAD request")
	}
	close(releaseHead)
	if err := <-headResult; err != nil {
		t.Fatal(err)
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin requests = %d, want independent HEAD and GET requests", got)
	}
}

func TestManagerRevalidatesExplicitlyStaleEntry(t *testing.T) {
	var origins atomic.Int64
	origin := originFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		if origins.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":  []string{"application/octet-stream"},
					"Cache-Control": []string{"max-age=0"},
					"ETag":          []string{`"asset-v1"`},
				},
				Body:          io.NopCloser(strings.NewReader("body")),
				ContentLength: 4,
			}, nil
		}
		if got := request.Header.Get("If-None-Match"); got != `"asset-v1"` {
			return nil, fmt.Errorf("If-None-Match = %q, want %q", got, `"asset-v1"`)
		}
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Header:     http.Header{"Cache-Control": []string{"max-age=60"}, "ETag": []string{`"asset-v1"`}},
			Body:       http.NoBody,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/revalidate.bin")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	first, err := manager.Fetch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := manager.Fetch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Body) != "body" || second.Entry.ETag != `"asset-v1"` {
		t.Fatalf("revalidated result = body %q, etag %q", second.Body, second.Entry.ETag)
	}
	if !second.Entry.CreatedAt.After(first.Entry.CreatedAt) {
		t.Fatal("304 response did not refresh cache freshness timestamp")
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin requests = %d, want initial fetch plus revalidation", got)
	}
}

func TestIsFreshHonorsExplicitDirectives(t *testing.T) {
	now := time.Unix(1000, 0)
	entry := CacheEntry{CreatedAt: now.Add(-2 * time.Second), Headers: http.Header{"Cache-Control": []string{"public, max-age=1"}}}
	if IsFresh(entry, now) {
		t.Fatal("max-age entry was reported fresh after expiration")
	}
	entry.Headers.Set("Cache-Control", "no-cache")
	if IsFresh(entry, now) {
		t.Fatal("no-cache entry was reported fresh")
	}
	entry.Headers = http.Header{"Content-Type": []string{"application/octet-stream"}}
	if !IsFresh(entry, now) {
		t.Fatal("entry without freshness directives was reported stale")
	}
}

func TestFreshnessDirectivesAreOrderIndependent(t *testing.T) {
	now := time.Unix(1000, 0)
	entry := CacheEntry{CreatedAt: now, Headers: make(http.Header)}
	for _, value := range []string{"max-age=3600, no-cache", "no-cache, max-age=3600"} {
		entry.Headers.Set("Cache-Control", value)
		if IsFresh(entry, now) {
			t.Fatalf("directive order %q was reported fresh", value)
		}
	}
	entry.Headers.Set("Cache-Control", "max-age=3600, must-revalidate")
	if !IsFresh(entry, now.Add(time.Second)) {
		t.Fatal("must-revalidate incorrectly made a fresh entry stale")
	}
	if IsFresh(entry, now.Add(3601*time.Second)) {
		t.Fatal("must-revalidate entry remained fresh after max-age")
	}
	entry.Headers.Set("Cache-Control", "no-store")
	if IsFresh(entry, now) || CanServeStale(entry) {
		t.Fatal("no-store entry was considered fresh or eligible for stale fallback")
	}
}

func TestFreshnessUsesDateAndAge(t *testing.T) {
	now := time.Unix(1000, 0)
	entry := CacheEntry{
		CreatedAt: now,
		Headers: http.Header{
			"Cache-Control": []string{"max-age=8"},
			"Date":          []string{now.Add(-10 * time.Second).UTC().Format(http.TimeFormat)},
			"Age":           []string{"5"},
		},
	}
	if IsFresh(entry, now.Add(5*time.Second)) {
		t.Fatal("Date/Age current age was not included in freshness calculation")
	}
}

func TestNoStoreResponseIsNotCached(t *testing.T) {
	var origins atomic.Int64
	origin := originFunc(func(context.Context, *http.Request) (*http.Response, error) {
		origins.Add(1)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/octet-stream"}, "Cache-Control": []string{"no-store"}},
			Body:          io.NopCloser(strings.NewReader("private")),
			ContentLength: 7,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/private.bin")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	if result, err := manager.Fetch(context.Background(), request); err != nil || result.Cacheable {
		t.Fatalf("no-store result = %#v, err = %v", result, err)
	}
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, hash, err := CanonicalKey(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := manager.ram.Get(hash); ok {
		t.Fatal("no-store response was retained in RAM")
	}
	if _, _, _, err := manager.disk.Read(hash); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-store response was written to disk: %v", err)
	}
	if _, err := manager.Fetch(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin requests = %d, want 2 for two no-store requests", got)
	}
}

func TestRequestNoCacheForcesRevalidation(t *testing.T) {
	var origins atomic.Int64
	origin := originFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		if origins.Add(1) == 1 {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"application/octet-stream"}, "Cache-Control": []string{"max-age=3600"}, "ETag": []string{`"v1"`}},
				Body:          io.NopCloser(strings.NewReader("body")),
				ContentLength: 4,
			}, nil
		}
		if got := request.Header.Get("If-None-Match"); got != `"v1"` {
			return nil, fmt.Errorf("If-None-Match = %q, want %q", got, `"v1"`)
		}
		return &http.Response{
			StatusCode: http.StatusNotModified,
			Header:     http.Header{"Cache-Control": []string{"max-age=3600"}, "ETag": []string{`"v1"`}},
			Body:       http.NoBody,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/forced.bin")
	firstRequest := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	if _, err := manager.Fetch(context.Background(), firstRequest); err != nil {
		t.Fatal(err)
	}
	secondRequest := &http.Request{Method: http.MethodGet, URL: target, Header: http.Header{"Cache-Control": []string{"no-cache"}}}
	if result, err := manager.Fetch(context.Background(), secondRequest); err != nil || string(result.Body) != "body" {
		t.Fatalf("forced revalidation result = %#v, err = %v", result, err)
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin requests = %d, want 2", got)
	}
}

func TestRootTransitionDrainsActiveWriters(t *testing.T) {
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: originFunc(func(context.Context, *http.Request) (*http.Response, error) {
		return nil, errors.New("not used")
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.writeGate.enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		release, err := manager.BeginRootTransition(context.Background())
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("root transition completed while writer was active: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	manager.writeGate.leave()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("root transition did not finish after writer drained")
	}
}

func TestStoreQueuesBehindRootTransitionWithoutBlocking(t *testing.T) {
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: originFunc(func(context.Context, *http.Request) (*http.Response, error) {
		return nil, errors.New("not used")
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.writeGate.begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	manager.store(strings.Repeat("a", 64), CacheEntry{Version: MetadataVersion, StatusCode: http.StatusOK}, []byte("body"), nil)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("store blocked behind root transition for %v", elapsed)
	}
	manager.writeGate.end()
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCachedHeadersDropSetCookie(t *testing.T) {
	origin := originFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/octet-stream"},
				"Set-Cookie":   []string{"session=private"},
			},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: 4,
		}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://static.example.test/cookie.bin")
	request := &http.Request{Method: http.MethodGet, URL: target, Header: make(http.Header)}
	result, err := manager.Fetch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Entry.Headers.Get("Set-Cookie") != "" {
		t.Fatal("Set-Cookie was retained in cache metadata")
	}
	recorder := httptest.NewRecorder()
	WriteResult(recorder, request, result)
	if recorder.Header().Get("Set-Cookie") != "" {
		t.Fatal("Set-Cookie was sent from cached response")
	}
}

func TestWalkSizeExcludesMigrationState(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"objects/aa", "metadata/aa", "state"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "objects", "aa", "object"), []byte("object"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata", "aa", "object.json"), []byte("meta"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "migration.json"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unmanaged.txt"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	var total int64
	if err := walkSize(root, &total); err != nil {
		t.Fatal(err)
	}
	if total != int64(len("object")+len("meta")) {
		t.Fatalf("cache size = %d, want %d without migration state", total, len("object")+len("meta"))
	}
}

type originFunc func(context.Context, *http.Request) (*http.Response, error)

func (f originFunc) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	return f(ctx, request)
}
