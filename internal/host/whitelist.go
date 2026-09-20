package host

import (
	"net"
	"strings"
)

// HostMatcher is intentionally narrow. A proxy request is allowed only when
// the normalized DNS name matches one of the configured exact names or an
// explicitly configured controlled suffix. There is no public-suffix wildcard.
type HostMatcher interface {
	Allowed(host string) bool
}

type Whitelist struct {
	exact      map[string]struct{}
	subdomains []string
}

func New(exactHosts []string, controlledSubdomains []string) *Whitelist {
	w := &Whitelist{exact: make(map[string]struct{})}
	for _, value := range exactHosts {
		if normalized, ok := normalize(value); ok {
			w.exact[normalized] = struct{}{}
		}
	}
	for _, value := range controlledSubdomains {
		if normalized, ok := normalize(value); ok {
			w.subdomains = append(w.subdomains, normalized)
		}
	}
	return w
}

func (w *Whitelist) Allowed(value string) bool {
	if w == nil {
		return false
	}
	hostname, ok := normalize(value)
	if !ok {
		return false
	}
	if _, exists := w.exact[hostname]; exists {
		return true
	}
	for _, suffix := range w.subdomains {
		if hostname == suffix || strings.HasSuffix(hostname, "."+suffix) {
			return true
		}
	}
	return false
}

func Normalize(value string) (string, bool) {
	return normalize(value)
}

func normalize(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "/?#@[]: \t\r\n\\") {
		return "", false
	}
	value = strings.TrimSuffix(strings.ToLower(value), ".")
	if value == "" || net.ParseIP(value) != nil {
		return "", false
	}
	if len(value) > 253 {
		return "", false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '-' {
			continue
		}
		// Reject Unicode and other ambiguous host representations. A future
		// IDNA implementation can be added at the boundary deliberately.
		return "", false
	}
	labels := strings.Split(value, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
	}
	return value, true
}
