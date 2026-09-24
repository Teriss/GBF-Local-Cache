package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/host"
	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/stats"
)

func streamingProxy(t *testing.T, origin OriginClient, maxBytes int64) (*http.Client, *logging.Ring) {
	t.Helper()
	logs := logging.NewRing(100)
	manager, err := cache.NewManager(cache.ManagerConfig{Root: t.TempDir(), Origin: origin, Logs: logs, MaxObjectBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewCachedServer(host.New([]string{"static.example.test"}, nil), origin, manager, nil, &stats.Stats{}, logs)
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(server)
	t.Cleanup(proxyServer.Close)
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}, logs
}

func TestColdMissStreamsBeforeDownloadAndSharesOrigin(t *testing.T) {
	const first = "abcdefghijklmnop"
	const second = "qrstuvwxyz012345"
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var origins atomic.Int64
	origin := roundTripFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		reader, writer := io.Pipe()
		go func() {
			_, _ = writer.Write([]byte(first))
			<-release
			_, _ = writer.Write([]byte(second))
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
			Body: reader, ContentLength: -1, Request: request}, nil
	})
	client, logs := streamingProxy(t, origin, 0)
	response, err := client.Get("http://static.example.test/scene.js")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	readFirst := make(chan error, 1)
	go func() {
		buffer := make([]byte, len(first))
		_, readErr := io.ReadFull(response.Body, buffer)
		if readErr == nil && string(buffer) != first {
			readErr = errors.New("incorrect first chunk")
		}
		readFirst <- readErr
	}()
	select {
	case err := <-readFirst:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("browser did not receive first chunk while origin was still blocked")
	}

	followerDone := make(chan error, 1)
	go func() {
		follower, getErr := client.Get("http://static.example.test/scene.js")
		if getErr != nil {
			followerDone <- getErr
			return
		}
		defer follower.Body.Close()
		body, readErr := io.ReadAll(follower.Body)
		if readErr == nil && string(body) != first+second {
			readErr = errors.New("incorrect shared body")
		}
		followerDone <- readErr
	}()
	time.Sleep(30 * time.Millisecond)
	if got := origins.Load(); got != 1 {
		t.Fatalf("origin calls while blocked = %d, want 1", got)
	}
	unblock()
	remaining, err := io.ReadAll(response.Body)
	if err != nil || string(remaining) != second {
		t.Fatalf("remaining body = %q, err = %v", remaining, err)
	}
	if err := <-followerDone; err != nil {
		t.Fatal(err)
	}
	if got := origins.Load(); got != 1 {
		t.Fatalf("origin calls = %d, want 1", got)
	}
	foundTiming := false
	deadline := time.Now().Add(time.Second)
	for !foundTiming && time.Now().Before(deadline) {
		for _, entry := range logs.Recent(0) {
			if entry.Category == logging.CategoryNetwork && strings.Contains(entry.Message, "origin_first_byte=") && strings.Contains(entry.Message, "browser_first_byte=") {
				foundTiming = true
			}
		}
		if !foundTiming {
			time.Sleep(time.Millisecond)
		}
	}
	if !foundTiming {
		t.Fatalf("cold request timing was not logged: %#v", logs.Recent(0))
	}
}

func TestColdMissDecodesGzipAndCachesDecodedBytes(t *testing.T) {
	const payload = "console.log('compressed asset')"
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	if _, err := zipper.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	var origins atomic.Int64
	origin := roundTripFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		if got := request.Header.Get("Accept-Encoding"); got != "gzip" {
			return nil, errors.New("origin request did not ask for gzip")
		}
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"application/javascript"}, "Content-Encoding": []string{"gzip"}, "Content-Length": []string{strconv.Itoa(compressed.Len())}, "Etag": []string{`"compressed-v1"`}},
			Body:   io.NopCloser(bytes.NewReader(compressed.Bytes())), ContentLength: int64(compressed.Len()), Request: request}, nil
	})
	client, _ := streamingProxy(t, origin, 0)
	for i := 0; i < 2; i++ {
		response, err := client.Get("http://static.example.test/app.js")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || string(body) != payload {
			t.Fatalf("decoded body = %q, err = %v", body, readErr)
		}
		if got := response.Header.Get("Content-Encoding"); got != "" {
			t.Fatalf("downstream content encoding = %q", got)
		}
		if got := response.Header.Get("Etag"); got != `W/"compressed-v1"` {
			t.Fatalf("downstream etag = %q", got)
		}
	}
	if got := origins.Load(); got != 1 {
		t.Fatalf("origin calls = %d, want 1", got)
	}
	rangeRequest, _ := http.NewRequest(http.MethodGet, "http://static.example.test/app.js", nil)
	rangeRequest.Header.Set("Range", "bytes=0-6")
	rangeResponse, err := client.Do(rangeRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer rangeResponse.Body.Close()
	rangeBody, err := io.ReadAll(rangeResponse.Body)
	if err != nil || rangeResponse.StatusCode != http.StatusPartialContent || string(rangeBody) != payload[:7] {
		t.Fatalf("decoded range = %d %q, err = %v", rangeResponse.StatusCode, rangeBody, err)
	}
}

func TestGzipRevalidationKeepsDecodedRepresentation(t *testing.T) {
	const payload = "console.log('revalidated')"
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	_, _ = zipper.Write([]byte(payload))
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	var origins atomic.Int64
	origin := roundTripFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		if origins.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"application/javascript"}, "Content-Encoding": []string{"gzip"},
					"Cache-Control": []string{"max-age=0"}, "Etag": []string{`"compressed-v1"`}},
				Body: io.NopCloser(bytes.NewReader(compressed.Bytes())), ContentLength: int64(compressed.Len()), Request: request}, nil
		}
		if got := request.Header.Get("If-None-Match"); got != `W/"compressed-v1"` {
			return nil, errors.New("revalidation did not use the weak cached validator")
		}
		return &http.Response{StatusCode: http.StatusNotModified,
			Header: http.Header{"Content-Encoding": []string{"gzip"}, "Cache-Control": []string{"max-age=60"}, "Etag": []string{`"compressed-v1"`}},
			Body:   http.NoBody, ContentLength: 0, Request: request}, nil
	})
	client, _ := streamingProxy(t, origin, 0)
	for i := 0; i < 3; i++ {
		response, err := client.Get("http://static.example.test/revalidate.js")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || string(body) != payload || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Etag") != `W/"compressed-v1"` {
			t.Fatalf("response %d: body=%q encoding=%q etag=%q err=%v", i, body, response.Header.Get("Content-Encoding"), response.Header.Get("Etag"), readErr)
		}
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}
}

func TestInterruptedStreamAndOversizeDoNotEnterCache(t *testing.T) {
	for _, test := range []struct {
		name string
		body io.ReadCloser
		max  int64
	}{
		{name: "truncated", body: io.NopCloser(&failAfterPrefix{}), max: 0},
		{name: "oversize", body: io.NopCloser(strings.NewReader("abcdefghijklmnopqrstuvwxyz012345")), max: 24},
	} {
		t.Run(test.name, func(t *testing.T) {
			var origins atomic.Int64
			origin := roundTripFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
				if origins.Add(1) == 1 {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
						Body: test.body, ContentLength: -1, Request: request}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
					Body: io.NopCloser(strings.NewReader("valid-second-request")), ContentLength: 20, Request: request}, nil
			})
			client, _ := streamingProxy(t, origin, test.max)
			response, err := client.Get("http://static.example.test/failure.js")
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil {
				t.Fatal("interrupted body was reported complete")
			}
			second, err := client.Get("http://static.example.test/failure.js")
			if err != nil {
				t.Fatal(err)
			}
			defer second.Body.Close()
			body, err := io.ReadAll(second.Body)
			if err != nil || string(body) != "valid-second-request" {
				t.Fatalf("second body = %q, err = %v", body, err)
			}
			if got := origins.Load(); got != 2 {
				t.Fatalf("origin calls = %d, want 2", got)
			}
		})
	}
}

func TestInvalidPrefixReturnsGatewayErrorWithoutCaching(t *testing.T) {
	var origins atomic.Int64
	origin := roundTripFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/javascript"}},
			Body: io.NopCloser(strings.NewReader("<!doctype html><body>error</body>")), ContentLength: -1, Request: request}, nil
	})
	client, _ := streamingProxy(t, origin, 0)
	for i := 0; i < 2; i++ {
		response, err := client.Get("http://static.example.test/bad.js")
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", response.StatusCode)
		}
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}
}

func TestNoStoreResponseStreamsButDoesNotEnterCache(t *testing.T) {
	var origins atomic.Int64
	origin := roundTripFunc(func(_ context.Context, request *http.Request) (*http.Response, error) {
		origins.Add(1)
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"application/javascript"}, "Cache-Control": []string{"no-store"}},
			Body:   io.NopCloser(strings.NewReader("console.log('fresh')")), ContentLength: 20, Request: request}, nil
	})
	client, _ := streamingProxy(t, origin, 0)
	for i := 0; i < 2; i++ {
		response, err := client.Get("http://static.example.test/no-store.js")
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || string(body) != "console.log('fresh')" {
			t.Fatalf("body = %q, err = %v", body, readErr)
		}
	}
	if got := origins.Load(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}
}

type failAfterPrefix struct{ read bool }

func (reader *failAfterPrefix) Read(p []byte) (int, error) {
	if reader.read {
		return 0, io.ErrUnexpectedEOF
	}
	reader.read = true
	return copy(p, "abcdefghijklmnop"), nil
}
