package cache

import (
	"context"
	"io"
	"net/http"
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
