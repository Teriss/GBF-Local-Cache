package network

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"gbf-local-cache/internal/config"
)

type Client interface {
	Do(context.Context, *http.Request) (*http.Response, error)
	CloseIdleConnections()
}

type clientHolder struct {
	client *http.Client
	close  func()
}

type Manager struct {
	current atomic.Pointer[clientHolder]
	mu      sync.Mutex
	config  config.Config
}

type TestResult struct {
	OK            bool   `json:"ok"`
	Host          string `json:"host"`
	Status        int    `json:"status"`
	DNSMs         int64  `json:"dns_ms"`
	TCPMs         int64  `json:"tcp_ms"`
	TLSMs         int64  `json:"tls_ms"`
	TTFBMs        int64  `json:"ttfb_ms"`
	DurationMs    int64  `json:"duration_ms"`
	DownloadBytes int64  `json:"download_bytes"`
	ThroughputBPS int64  `json:"throughput_bps"`
	HTTPVersion   string `json:"http_version"`
	Error         string `json:"error,omitempty"`
}

func NewManager(cfg config.Config) (*Manager, error) {
	holder, err := buildHolder(cfg)
	if err != nil {
		return nil, err
	}
	manager := &Manager{config: cfg}
	manager.current.Store(holder)
	return manager, nil
}

func (m *Manager) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	holder := m.current.Load()
	if holder == nil || holder.client == nil {
		return nil, errors.New("network client is not initialized")
	}
	return holder.client.Do(request.WithContext(ctx))
}

func (m *Manager) CloseIdleConnections() {
	holder := m.current.Load()
	if holder != nil && holder.close != nil {
		holder.close()
	}
}

func (m *Manager) Config() config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config
}

// Switch builds and tests a new transport before replacing the current one.
// Cache state is intentionally not touched by the network swap.
func (m *Manager) Switch(ctx context.Context, cfg config.Config, testHost string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	holder, err := buildHolder(cfg)
	if err != nil {
		return err
	}
	if testHost != "" {
		result := testClient(ctx, holder.client, testHost)
		if !result.OK {
			holder.close()
			if result.Error == "" {
				return fmt.Errorf("network test failed with HTTP status %d", result.Status)
			}
			return errors.New(result.Error)
		}
	}
	old := m.current.Swap(holder)
	m.mu.Lock()
	m.config = cfg
	m.mu.Unlock()
	if old != nil && old.close != nil {
		old.close()
	}
	return nil
}

func (m *Manager) Test(ctx context.Context, host string) TestResult {
	if ctx == nil {
		ctx = context.Background()
	}
	holder := m.current.Load()
	if holder == nil {
		return TestResult{Host: host, Error: "network client is not initialized"}
	}
	return testClient(ctx, holder.client, host)
}

func buildHolder(cfg config.Config) (*clientHolder, error) {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   12 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	switch cfg.NetworkMode {
	case config.NetworkModeDirect:
		// Proxy remains explicitly nil. This is the UU/system-direct mode.
	case config.NetworkModeClash:
		protocol := strings.ToLower(strings.TrimSpace(cfg.Clash.Protocol))
		if protocol == "" {
			protocol = "http"
		}
		address := net.JoinHostPort(cfg.Clash.Host, strconv.Itoa(cfg.Clash.Port))
		switch protocol {
		case "http":
			proxyURL := &url.URL{Scheme: "http", Host: address}
			if cfg.Clash.Username != "" {
				proxyURL.User = url.UserPassword(cfg.Clash.Username, cfg.Clash.Password)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
		case "socks5":
			auth := &proxy.Auth{User: cfg.Clash.Username, Password: cfg.Clash.Password}
			var dialer proxy.Dialer
			var err error
			if cfg.Clash.Username == "" && cfg.Clash.Password == "" {
				dialer, err = proxy.SOCKS5("tcp", address, nil, &net.Dialer{Timeout: 10 * time.Second})
			} else {
				dialer, err = proxy.SOCKS5("tcp", address, auth, &net.Dialer{Timeout: 10 * time.Second})
			}
			if err != nil {
				return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
			}
			transport.DialContext = func(ctx context.Context, networkName, destination string) (net.Conn, error) {
				return dialer.Dial(networkName, destination)
			}
		default:
			return nil, fmt.Errorf("unsupported Clash protocol %q", protocol)
		}
	default:
		return nil, fmt.Errorf("unsupported network mode %q", cfg.NetworkMode)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   45 * time.Second,
		// Redirects must be returned to the browser. Following a Location
		// here could make the origin client connect to a host that was not
		// admitted by the local CDN whitelist.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &clientHolder{client: client, close: transport.CloseIdleConnections}, nil
}

func testClient(ctx context.Context, client *http.Client, host string) TestResult {
	result := TestResult{Host: host}
	if client == nil {
		result.Error = "network client is nil"
		return result
	}
	started := time.Now()
	var dnsStart, connectStart, tlsStart, firstByte time.Time
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				result.DNSMs = time.Since(dnsStart).Milliseconds()
			}
		},
		ConnectStart: func(_, _ string) { connectStart = time.Now() },
		ConnectDone: func(_, _ string, _ error) {
			if !connectStart.IsZero() {
				result.TCPMs = time.Since(connectStart).Milliseconds()
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			if !tlsStart.IsZero() {
				result.TLSMs = time.Since(tlsStart).Milliseconds()
			}
		},
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	testContext, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(testContext, http.MethodGet, "https://"+host+"/", nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	request.Header.Set("Range", "bytes=0-65535")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		result.DurationMs = time.Since(started).Milliseconds()
		result.Error = err.Error()
		return result
	}
	defer response.Body.Close()
	result.Status = response.StatusCode
	result.HTTPVersion = response.Proto
	if !firstByte.IsZero() {
		result.TTFBMs = firstByte.Sub(started).Milliseconds()
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	result.DownloadBytes = int64(len(data))
	result.DurationMs = time.Since(started).Milliseconds()
	if result.DurationMs > 0 {
		result.ThroughputBPS = result.DownloadBytes * 1000 / result.DurationMs
	}
	if readErr != nil {
		result.Error = readErr.Error()
		return result
	}
	result.OK = true
	return result
}
