package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// CanonicalKey is deliberately based on the complete resource URL. Query
// parameters are normalized and sorted, never discarded, so signed CDN URLs
// and version parameters cannot collide.
func CanonicalKey(resource *url.URL) (canonical string, hash string, err error) {
	if resource == nil || resource.Hostname() == "" {
		return "", "", fmt.Errorf("resource URL must contain a host")
	}
	scheme := strings.ToLower(strings.TrimSpace(resource.Scheme))
	if scheme != "http" && scheme != "https" {
		return "", "", fmt.Errorf("unsupported URL scheme %q", resource.Scheme)
	}
	hostname := strings.TrimSuffix(strings.ToLower(resource.Hostname()), ".")
	if hostname == "" {
		return "", "", fmt.Errorf("resource URL host is empty")
	}
	port := resource.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, parseErr := strconv.Atoi(port)
	if parseErr != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("invalid URL port %q", port)
	}

	path := resource.EscapedPath()
	if path == "" {
		path = "/"
	}
	query, queryErr := normalizeQuery(resource.RawQuery)
	if queryErr != nil {
		return "", "", queryErr
	}
	canonical = scheme + "://" + hostname + ":" + port + path
	if query != "" {
		canonical += "?" + query
	}
	digest := sha256.Sum256([]byte(canonical))
	return canonical, hex.EncodeToString(digest[:]), nil
}

func normalizeQuery(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "", fmt.Errorf("normalize query: %w", err)
	}
	for key := range values {
		sort.Strings(values[key])
	}
	return values.Encode(), nil
}
