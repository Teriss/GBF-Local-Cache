package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/cert"
	"gbf-local-cache/internal/host"
	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/stats"
)

func TestPlainHTTPProxyForwardsOnlyWhitelistedStaticHosts(t *testing.T) {
	var originRequests atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		originRequests.Add(1)
		if request.Method == http.MethodHead {
			writer.Header().Set("Content-Length", "5")
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		_, _ = writer.Write([]byte("hello"))
	}))
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	const targetHost = "static.example.test"
	matcher := host.New([]string{targetHost}, nil)
	counters := &stats.Stats{}
	logs := logging.NewRing(20)
	originClient := roundTripFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		// Keep the request's public target host for whitelist testing while
		// routing the fake origin to httptest's loopback listener.
		clone := request.Clone(ctx)
		clone.URL.Host = originURL.Host
		clone.Host = originURL.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	proxyServer, err := NewServer(matcher, originClient, counters, logs)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://"+targetHost+"/assets/hero.png?version=1", nil)
	response := httptest.NewRecorder()
	proxyServer.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if response.Body.String() != "hello" {
		t.Fatalf("proxy body = %q, want hello", response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("origin requests = %d, want 1", got)
	}

	blocked := httptest.NewRecorder()
	blockedRequest := httptest.NewRequest(http.MethodGet, "http://not-allowed.example/assets.js", nil)
	proxyServer.ServeHTTP(blocked, blockedRequest)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("blocked status = %d, want 403", blocked.Code)
	}
	if got := originRequests.Load(); got != 1 {
		t.Fatalf("blocked request reached origin: %d requests", got)
	}

	head := httptest.NewRecorder()
	headRequest := httptest.NewRequest(http.MethodHead, "http://"+targetHost+"/assets/hero.png", nil)
	proxyServer.ServeHTTP(head, headRequest)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD response = status %d, body %q", head.Code, head.Body.String())
	}
	if got := originRequests.Load(); got != 2 {
		t.Fatalf("origin requests after HEAD = %d, want 2", got)
	}

	connect := httptest.NewRecorder()
	connectRequest := httptest.NewRequest(http.MethodConnect, "http://not-allowed.example:443", nil)
	connectRequest.Host = "not-allowed.example:443"
	proxyServer.ServeHTTP(connect, connectRequest)
	if connect.Code != http.StatusForbidden {
		t.Fatalf("blocked CONNECT status = %d, want 403", connect.Code)
	}
}

func TestDirectOriginClientDoesNotFollowRedirects(t *testing.T) {
	var destinationRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		destinationRequests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()

	client := NewDirectOriginClient(time.Second)
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, redirect.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want %d", response.StatusCode, http.StatusFound)
	}
	if got := destinationRequests.Load(); got != 0 {
		t.Fatalf("redirect destination received %d requests", got)
	}
}

func TestListenerRoundTripUsesLoopbackOnlyProxy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("fake cdn"))
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	const targetHost = "static.example.test"
	originClient := roundTripFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		clone := request.Clone(ctx)
		clone.URL.Host = originURL.Host
		clone.Host = originURL.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	core, err := NewServer(host.New([]string{targetHost}, nil), originClient, &stats.Stats{}, logging.NewRing(20))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := StartListener("127.0.0.1:0", core)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = listener.Shutdown(context.Background())
	}()

	proxyURL, err := url.Parse("http://" + listener.Address())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return proxyURL, nil },
	}}
	response, err := client.Get("http://" + targetHost + "/assets/test.js")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "fake cdn" {
		t.Fatalf("round trip = %d %q", response.StatusCode, body)
	}
}

func TestPlainHTTPProxyRejectsMethodsOutsideStaticScope(t *testing.T) {
	matcher := host.New([]string{"static.example.test"}, nil)
	proxyServer, err := NewServer(matcher, roundTripFunc(func(ctx context.Context, req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("origin should not be called")
	}), &stats.Stats{}, logging.NewRing(10))
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://static.example.test/data", strings.NewReader("no business API"))
	response := httptest.NewRecorder()
	proxyServer.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", response.Code)
	}
}

func TestUnknownHTTPSHostIsRejectedBeforeProtocolHandling(t *testing.T) {
	matcher := host.New([]string{"static.example.test"}, nil)
	proxyServer, err := NewServer(matcher, roundTripFunc(func(context.Context, *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("origin should not be called")
	}), &stats.Stats{}, logging.NewRing(10))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://google.com/assets.js", nil)
	response := httptest.NewRecorder()
	proxyServer.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unknown HTTPS host status = %d, want 403", response.Code)
	}
}

func TestStartListenerRejectsNonLoopback(t *testing.T) {
	if _, err := StartListener("0.0.0.0:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Fatal("StartListener accepted 0.0.0.0")
	}
}

type roundTripFunc func(context.Context, *http.Request) (*http.Response, error)

func (f roundTripFunc) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return f(ctx, req)
}

func TestCopyResponseBodyIsNotLoggedWithQuery(t *testing.T) {
	matcher := host.New([]string{"static.example.test"}, nil)
	logs := logging.NewRing(10)
	responseBody := io.NopCloser(strings.NewReader("ok"))
	proxyServer, err := NewServer(matcher, roundTripFunc(func(context.Context, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: responseBody}, nil
	}), &stats.Stats{}, logs)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://static.example.test/private?token=secret", nil)
	recorder := httptest.NewRecorder()
	proxyServer.ServeHTTP(recorder, request)
	for _, entry := range logs.Snapshot() {
		if strings.Contains(entry.Target, "secret") {
			t.Fatalf("log leaked query string: %#v", entry)
		}
	}
}

func TestCachedProxySupportsHitValidatorsAndRange(t *testing.T) {
	var originRequests atomic.Int64
	origin := roundTripFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		originRequests.Add(1)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/javascript"}, "Etag": []string{`"v1"`}, "Last-Modified": []string{"Wed, 21 Oct 2015 07:28:00 GMT"}},
			Body:          io.NopCloser(strings.NewReader("0123456789")),
			ContentLength: 10,
			Request:       request,
		}, nil
	})
	manager, err := cache.NewManager(cache.ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewCachedServer(host.New([]string{"static.example.test"}, nil), origin, manager, nil, &stats.Stats{}, logging.NewRing(30))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://static.example.test/audio.mp3", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "0123456789" {
		t.Fatalf("first response = %d %q", response.Code, response.Body.String())
	}
	if originRequests.Load() != 1 {
		t.Fatalf("origin requests after miss = %d", originRequests.Load())
	}

	notModified := httptest.NewRecorder()
	conditional := httptest.NewRequest(http.MethodGet, "http://static.example.test/audio.mp3", nil)
	conditional.Header.Set("If-None-Match", `"v1"`)
	server.ServeHTTP(notModified, conditional)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("validator response = %d %q", notModified.Code, notModified.Body.String())
	}
	if originRequests.Load() != 1 {
		t.Fatalf("validator reached origin: %d", originRequests.Load())
	}

	rangeResponse := httptest.NewRecorder()
	rangeRequest := httptest.NewRequest(http.MethodGet, "http://static.example.test/audio.mp3", nil)
	rangeRequest.Header.Set("Range", "bytes=2-5")
	server.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "2345" || rangeResponse.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range response = %d %q %q", rangeResponse.Code, rangeResponse.Body.String(), rangeResponse.Header().Get("Content-Range"))
	}
}

func TestWhitelistedConnectUsesTLSMITMAndNeverTunnelsUnknownHost(t *testing.T) {
	certificateManager := cert.New(t.TempDir())
	if err := certificateManager.Ensure(); err != nil {
		t.Fatal(err)
	}
	var originRequests atomic.Int64
	origin := roundTripFunc(func(ctx context.Context, request *http.Request) (*http.Response, error) {
		originRequests.Add(1)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/javascript"}},
			Body:          io.NopCloser(strings.NewReader("mitm-ok")),
			ContentLength: 7,
			Request:       request,
		}, nil
	})
	manager, err := cache.NewManager(cache.ManagerConfig{Root: t.TempDir(), Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	logs := logging.NewRing(30)
	counters := &stats.Stats{}
	server, err := NewCachedServer(host.New([]string{"static.example.test"}, nil), origin, manager, certificateManager, counters, logs)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := StartListener("127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Shutdown(context.Background()) }()

	certPEM, err := os.ReadFile(filepath.Join(certificateManagerDirectory(certificateManager), "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	proxyURL, _ := url.Parse("http://" + listener.Address())
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}, Timeout: 10 * time.Second}
	response, err := client.Get("https://static.example.test/assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "mitm-ok" {
		t.Fatalf("MITM response = %d %q", response.StatusCode, body)
	}
	if originRequests.Load() != 1 {
		t.Fatalf("origin requests = %d", originRequests.Load())
	}
}

func certificateManagerDirectory(manager *cert.Manager) string {
	status := manager.Status()
	return status.Directory
}
