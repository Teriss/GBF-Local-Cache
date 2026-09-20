package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gbf-local-cache/internal/cache"
	"gbf-local-cache/internal/cert"
	"gbf-local-cache/internal/host"
	"gbf-local-cache/internal/logging"
	"gbf-local-cache/internal/stats"
)

type OriginClient interface {
	Do(ctx context.Context, req *http.Request) (*http.Response, error)
}

type HTTPOriginClient struct {
	client *http.Client
}

func NewDirectOriginClient(timeout time.Duration) *HTTPOriginClient {
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &HTTPOriginClient{client: &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *HTTPOriginClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("origin client is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return c.client.Do(req.WithContext(ctx))
}

func (c *HTTPOriginClient) CloseIdleConnections() {
	if c == nil || c.client == nil {
		return
	}
	if transport, ok := c.client.Transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

type Server struct {
	whitelist host.HostMatcher
	origin    OriginClient
	cache     *cache.Manager
	ca        *cert.Manager
	stats     *stats.Stats
	logs      *logging.Ring
}

// NewServer keeps the direct, uncached constructor available for focused
// forwarding tests. Production uses NewCachedServer.
// Production uses NewCachedServer so every request passes through the cache
// pipeline and the TLS MITM handler.
func NewServer(matcher host.HostMatcher, origin OriginClient, counters *stats.Stats, logs *logging.Ring) (*Server, error) {
	return newServer(matcher, origin, nil, nil, counters, logs)
}

func NewCachedServer(matcher host.HostMatcher, origin OriginClient, manager *cache.Manager, caManager *cert.Manager, counters *stats.Stats, logs *logging.Ring) (*Server, error) {
	return newServer(matcher, origin, manager, caManager, counters, logs)
}

func newServer(matcher host.HostMatcher, origin OriginClient, manager *cache.Manager, caManager *cert.Manager, counters *stats.Stats, logs *logging.Ring) (*Server, error) {
	if matcher == nil {
		return nil, errors.New("proxy whitelist is required")
	}
	if origin == nil {
		return nil, errors.New("proxy origin client is required")
	}
	if counters == nil {
		counters = &stats.Stats{}
	}
	if logs == nil {
		logs = logging.NewRing(5000)
	}
	return &Server{whitelist: matcher, origin: origin, cache: manager, ca: caManager, stats: counters, logs: logs}, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	s.stats.Request()

	if request.Method == http.MethodConnect {
		s.handleConnect(writer, request)
		return
	}
	target, err := proxyTarget(request)
	if err != nil {
		s.log(logging.CategoryError, request.Method, request.URL, err.Error(), started)
		s.stats.Error()
		http.Error(writer, "bad proxy request", http.StatusBadRequest)
		return
	}
	if target.User != nil {
		s.log(logging.CategoryError, request.Method, target, "userinfo in proxy target is not allowed", started)
		s.stats.Error()
		http.Error(writer, "userinfo is not allowed", http.StatusBadRequest)
		return
	}
	if !s.whitelist.Allowed(target.Hostname()) {
		s.log(logging.CategoryError, request.Method, target, "target host is not in the static CDN whitelist", started)
		s.stats.Error()
		writeForbidden(writer)
		return
	}
	if !strings.EqualFold(target.Scheme, "http") && !strings.EqualFold(target.Scheme, "https") {
		s.stats.Error()
		http.Error(writer, "unsupported proxy scheme", http.StatusNotImplemented)
		return
	}
	if port := target.Port(); port != "" && ((strings.EqualFold(target.Scheme, "http") && port != "80") || (strings.EqualFold(target.Scheme, "https") && port != "443")) {
		s.stats.Error()
		s.log(logging.CategoryError, request.Method, target, "non-standard CDN port is not allowed", started)
		writeForbidden(writer)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		s.log(logging.CategoryError, request.Method, target, "method is outside static-cache scope", started)
		s.stats.Error()
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.stats.CacheableRequest()
	outbound := cloneOriginRequest(request, target)
	if s.cache != nil {
		result, err := s.cache.Fetch(request.Context(), outbound)
		if err != nil {
			s.stats.Error()
			s.log(logging.CategoryError, request.Method, target, "cache pipeline failed: "+err.Error(), started)
			if request.Context().Err() != nil {
				return
			}
			http.Error(writer, "bad gateway", http.StatusBadGateway)
			return
		}
		if request.Header.Get("Range") != "" {
			s.stats.RangeRequest()
			s.log(logging.CategoryRange, request.Method, target, "range response", started)
		}
		cache.WriteResult(writer, request, result)
		s.logResponse(request, target, result.Entry.StatusCode, int64(len(result.Body)), started, string(result.Source))
		return
	}

	response, err := s.origin.Do(request.Context(), outbound)
	if err != nil {
		s.log(logging.CategoryError, request.Method, target, "origin request failed: "+err.Error(), started)
		s.stats.Error()
		if request.Context().Err() != nil {
			return
		}
		http.Error(writer, "bad gateway", http.StatusBadGateway)
		return
	}
	if response == nil || response.Body == nil {
		s.log(logging.CategoryError, request.Method, target, "origin returned an empty response", started)
		s.stats.Error()
		http.Error(writer, "bad gateway", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodHead {
		s.logResponse(request, target, response.StatusCode, 0, started, "origin")
		return
	}
	bytes, copyErr := io.Copy(writer, response.Body)
	s.stats.AddOriginBytes(bytes)
	s.logResponse(request, target, response.StatusCode, bytes, started, "origin")
	if copyErr != nil && request.Context().Err() == nil {
		s.stats.Error()
		s.log(logging.CategoryError, request.Method, target, "response copy failed: "+copyErr.Error(), started)
	}
}

func (s *Server) handleConnect(writer http.ResponseWriter, request *http.Request) {
	hostname, port, err := net.SplitHostPort(request.Host)
	if err != nil || port == "" {
		s.stats.Error()
		http.Error(writer, "CONNECT target must be host:port", http.StatusBadRequest)
		return
	}
	if port != "443" || !s.whitelist.Allowed(hostname) {
		s.stats.Error()
		s.log(logging.CategoryError, request.Method, &url.URL{Host: request.Host}, "CONNECT target is not in the static CDN whitelist", time.Now())
		writeForbidden(writer)
		return
	}
	if s.ca == nil {
		// A whitelisted CONNECT is only useful once TLS MITM is configured.
		// Never turn this into a generic forward tunnel.
		s.stats.Error()
		http.Error(writer, "HTTPS MITM is not enabled", http.StatusNotImplemented)
		return
	}
	if err := s.serveMITM(writer, request, hostname); err != nil {
		s.stats.Error()
		s.log(logging.CategoryError, request.Method, &url.URL{Host: request.Host}, "TLS MITM failed: "+err.Error(), time.Now())
	}
}

func (s *Server) serveMITM(writer http.ResponseWriter, request *http.Request, hostname string) error {
	certificate, err := s.ca.CertificateFor(hostname)
	if err != nil {
		return err
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return errors.New("HTTP server does not support connection hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return err
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = connection.Close()
		return err
	}
	if err := buffered.Flush(); err != nil {
		_ = connection.Close()
		return err
	}
	mitmConnection := net.Conn(connection)
	if buffered.Reader.Buffered() > 0 {
		mitmConnection = &bufferedConn{Conn: connection, reader: buffered.Reader}
	}
	tlsConnection := tls.Server(mitmConnection, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
		ServerName:   hostname,
	})
	if err := tlsConnection.HandshakeContext(request.Context()); err != nil {
		_ = tlsConnection.Close()
		return err
	}
	s.log(logging.CategoryCert, http.MethodConnect, &url.URL{Host: hostname}, "TLS MITM handshake complete", time.Now())
	inner := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	// The one-connection listener intentionally returns after handing the
	// TLS connection to net/http. Its Close method must not close the hijacked
	// connection: http.Server calls Listener.Close when its Accept loop ends,
	// while the connection handler may still be reading the first request.
	_ = inner.Serve(&singleConnListener{connection: tlsConnection})
	return nil
}

func proxyTarget(request *http.Request) (*url.URL, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("request URL is missing")
	}
	if request.URL.IsAbs() {
		if request.URL.Host == "" {
			return nil, errors.New("absolute URL has no host")
		}
		return request.URL, nil
	}
	if request.Host == "" {
		return nil, errors.New("origin-form request has no Host header")
	}
	path := request.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return url.Parse(scheme + "://" + request.Host + path)
}

func cloneOriginRequest(request *http.Request, target *url.URL) *http.Request {
	clone := request.Clone(request.Context())
	clone.URL = target
	clone.RequestURI = ""
	clone.Host = target.Host
	clone.Header = request.Header.Clone()
	for _, key := range []string{
		"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
		"TE", "Trailer", "Transfer-Encoding", "Upgrade", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	} {
		clone.Header.Del(key)
	}
	clone.Header.Del("Authorization")
	clone.Header.Del("Cookie")
	return clone
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func (s *Server) logResponse(request *http.Request, target *url.URL, status int, bytes int64, started time.Time, source string) {
	s.log(logging.CategoryNetwork, request.Method, target, fmt.Sprintf("%s %d, %d bytes", source, status, bytes), started)
}

func (s *Server) log(category logging.Category, method string, target *url.URL, message string, started time.Time) {
	s.logs.Add(logging.Entry{
		Time:     time.Now(),
		Category: category,
		Method:   method,
		Target:   safeTarget(target),
		Message:  message,
		Duration: time.Since(started).Round(time.Microsecond).String(),
	})
}

func safeTarget(target *url.URL) string {
	if target == nil {
		return ""
	}
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	return target.Host + path
}

func writeForbidden(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(writer, "target host is not allowed", http.StatusForbidden)
}

type singleConnListener struct {
	connection net.Conn
	once       sync.Once
}

var errSingleConnectionServed = errors.New("single connection already served")

func (l *singleConnListener) Accept() (net.Conn, error) {
	var connection net.Conn
	l.once.Do(func() { connection = l.connection })
	if connection != nil {
		return connection, nil
	}
	return nil, errSingleConnectionServed
}

func (l *singleConnListener) Close() error {
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.connection != nil {
		return l.connection.LocalAddr()
	}
	return &net.TCPAddr{}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
