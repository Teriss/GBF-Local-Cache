package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

type Listener struct {
	server *http.Server
	ln     net.Listener
	done   chan struct{}
}

func StartListener(address string, handler http.Handler) (*Listener, error) {
	if err := validateLoopbackAddress(address); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, errors.New("proxy handler is required")
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	listener := &Listener{server: server, ln: ln, done: make(chan struct{})}
	go func() {
		defer close(listener.done)
		if serveErr := server.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			// Serve errors are exposed through Wait; the service owns the
			// lifecycle and decides whether to transition to Error.
		}
	}()
	return listener, nil
}

func (l *Listener) Address() string {
	if l == nil || l.ln == nil {
		return ""
	}
	return l.ln.Addr().String()
}

func (l *Listener) Shutdown(ctx context.Context) error {
	if l == nil || l.server == nil {
		return nil
	}
	err := l.server.Shutdown(ctx)
	<-l.done
	return err
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid proxy listen address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("proxy must listen on a loopback IP, got %q", host)
	}
	return nil
}
