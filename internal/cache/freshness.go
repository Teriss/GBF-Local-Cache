package cache

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type cacheControlDirectives struct {
	noStore         bool
	noCache         bool
	mustRevalidate  bool
	proxyRevalidate bool
	maxAge          int64
	sMaxAge         int64
	hasMaxAge       bool
	hasSMaxAge      bool
	invalidAge      bool
}

func parseCacheControl(value string) cacheControlDirectives {
	var directives cacheControlDirectives
	for _, raw := range strings.Split(value, ",") {
		parts := strings.SplitN(strings.TrimSpace(raw), "=", 2)
		name := strings.ToLower(strings.Trim(strings.TrimSpace(parts[0]), `"`))
		if name == "" {
			continue
		}
		switch name {
		case "no-store":
			directives.noStore = true
		case "no-cache":
			directives.noCache = true
		case "must-revalidate":
			directives.mustRevalidate = true
		case "proxy-revalidate":
			directives.proxyRevalidate = true
		case "max-age", "s-maxage":
			if len(parts) != 2 {
				directives.invalidAge = true
				continue
			}
			seconds, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(parts[1]), `"`), 10, 64)
			if err != nil || seconds < 0 {
				directives.invalidAge = true
				continue
			}
			if name == "max-age" {
				if !directives.hasMaxAge || seconds < directives.maxAge {
					directives.maxAge = seconds
				}
				directives.hasMaxAge = true
			} else {
				if !directives.hasSMaxAge || seconds < directives.sMaxAge {
					directives.sMaxAge = seconds
				}
				directives.hasSMaxAge = true
			}
		}
	}
	return directives
}

// CanStoreResponse reports whether an origin response may enter either cache
// tier. no-store is checked before writing RAM as well as disk; otherwise a
// private response would remain available from the in-process cache.
func CanStoreResponse(headers http.Header) bool {
	return !parseCacheControl(headers.Get("Cache-Control")).noStore
}

// RequestDisallowsStore applies the request-side no-store directive to the
// proxy's own cache. A request no-cache still permits storing the revalidated
// representation, while request no-store does not.
func RequestDisallowsStore(request *http.Request) bool {
	if request == nil {
		return false
	}
	return parseCacheControl(request.Header.Get("Cache-Control")).noStore
}

// RequestForcesRevalidate implements the request directives that bypass a
// fresh cached representation. This is intentionally separate from IsFresh:
// freshness belongs to the stored response, while revalidation can be forced
// by each individual request.
func RequestForcesRevalidate(request *http.Request) bool {
	if request == nil {
		return false
	}
	directives := parseCacheControl(request.Header.Get("Cache-Control"))
	if directives.noCache || directives.noStore || (directives.hasMaxAge && directives.maxAge == 0) {
		return true
	}
	return strings.Contains(strings.ToLower(request.Header.Get("Pragma")), "no-cache")
}

// CanServeStale reports whether a failed revalidation may fall back to the
// stored response. Revalidation directives are not the same as freshness: a
// must-revalidate response may be served while fresh, but never after its
// freshness lifetime has expired and the origin is unavailable.
func CanServeStale(entry CacheEntry) bool {
	directives := parseCacheControl(entry.Headers.Get("Cache-Control"))
	return !directives.noStore && !directives.noCache && !directives.mustRevalidate && !directives.proxyRevalidate
}

// IsFresh applies HTTP freshness directives. Resources without an explicit
// freshness lifetime remain fresh indefinitely; this matches versioned GBF
// static assets while still honoring no-cache, max-age, Expires, Date, and Age.
func IsFresh(entry CacheEntry, now time.Time) bool {
	directives := parseCacheControl(entry.Headers.Get("Cache-Control"))
	if directives.noStore || directives.noCache || directives.invalidAge {
		return false
	}
	if directives.hasSMaxAge || directives.hasMaxAge {
		if entry.CreatedAt.IsZero() {
			return false
		}
		lifetime := directives.maxAge
		if directives.hasSMaxAge {
			lifetime = directives.sMaxAge
		}
		return cacheCurrentAge(entry, now) < time.Duration(lifetime)*time.Second
	}
	if expires := entry.Headers.Get("Expires"); expires != "" {
		expiresAt, err := http.ParseTime(expires)
		if err == nil {
			if date := parseHTTPTime(entry.Headers.Get("Date")); !date.IsZero() && expiresAt.After(date) {
				return cacheCurrentAge(entry, now) < expiresAt.Sub(date)
			}
			return now.Before(expiresAt)
		}
	}
	return true
}

func cacheCurrentAge(entry CacheEntry, now time.Time) time.Duration {
	resident := time.Duration(0)
	if !entry.CreatedAt.IsZero() && now.After(entry.CreatedAt) {
		resident = now.Sub(entry.CreatedAt)
	}
	correctedAge := time.Duration(0)
	if date := parseHTTPTime(entry.Headers.Get("Date")); !date.IsZero() && entry.CreatedAt.After(date) {
		if apparent := entry.CreatedAt.Sub(date); apparent > correctedAge {
			correctedAge = apparent
		}
	}
	if seconds, err := strconv.ParseInt(strings.TrimSpace(entry.Headers.Get("Age")), 10, 64); err == nil && seconds >= 0 {
		if serverAge := time.Duration(seconds) * time.Second; serverAge > correctedAge {
			correctedAge = serverAge
		}
	}
	return correctedAge + resident
}

func parseHTTPTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
