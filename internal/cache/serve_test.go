package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCanceledBrowserCompletesBoundedCacheFill(t *testing.T) {
	const payload = "abcdefghijklmnop-second-chunk"
	var origins atomic.Int64
	origin := originFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
			Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload)), Request: request}, nil
	})
	manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://static.example.test/scene.js", nil)
	broken := &failSecondWrite{header: make(http.Header)}
	outcome, err := manager.Serve(broken, request)
	if err != nil || !outcome.Committed || !outcome.ClientDisconnected || !outcome.Result.Cacheable {
		t.Fatalf("canceled serve: committed=%t disconnected=%t cacheable=%t err=%v", outcome.Committed, outcome.ClientDisconnected, outcome.Result.Cacheable, err)
	}

	response := httptest.NewRecorder()
	if _, err := manager.Serve(response, request); err != nil {
		t.Fatal(err)
	}
	if response.Body.String() != payload || origins.Load() != 1 {
		t.Fatalf("retry body=%q origins=%d", response.Body.String(), origins.Load())
	}
}

func TestCanceledBrowserDoesNotStoreIncompleteFill(t *testing.T) {
	for _, test := range []struct {
		name          string
		firstBody     string
		contentLength int64
	}{
		{name: "truncated", firstBody: "abcdefghijklmnop-second-chunk", contentLength: 100},
		{name: "detached_size_limit", firstBody: strings.Repeat("x", int(disconnectedFillLimit)+1), contentLength: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var origins atomic.Int64
			origin := originFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
				if origins.Add(1) == 1 {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
						Body: io.NopCloser(strings.NewReader(test.firstBody)), ContentLength: test.contentLength, Request: request}, nil
				}
				const valid = "valid-second-request"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
					Body: io.NopCloser(strings.NewReader(valid)), ContentLength: int64(len(valid)), Request: request}, nil
			})
			manager, err := NewManager(ManagerConfig{Root: t.TempDir(), Origin: origin})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://static.example.test/scene.js", nil)
			broken := &failSecondWrite{header: make(http.Header)}
			outcome, err := manager.Serve(broken, request)
			if err == nil || !outcome.ClientDisconnected || outcome.Result.Cacheable {
				t.Fatalf("incomplete fill: disconnected=%t cacheable=%t err=%v", outcome.ClientDisconnected, outcome.Result.Cacheable, err)
			}
			response := httptest.NewRecorder()
			if _, err := manager.Serve(response, request); err != nil {
				t.Fatal(err)
			}
			if response.Body.String() != "valid-second-request" || origins.Load() != 2 {
				t.Fatalf("retry body=%q origins=%d", response.Body.String(), origins.Load())
			}
		})
	}
}

type failSecondWrite struct {
	header http.Header
	writes int
}

func (writer *failSecondWrite) Header() http.Header { return writer.header }

func (writer *failSecondWrite) WriteHeader(int) {}

func (writer *failSecondWrite) Write(p []byte) (int, error) {
	writer.writes++
	if writer.writes == 2 {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}
