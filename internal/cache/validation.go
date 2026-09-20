package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func newEntry(target *url.URL, response *http.Response, body []byte) CacheEntry {
	headers := SanitizeHeaders(response.Header)
	if headers == nil {
		headers = make(http.Header)
	}
	contentType := headers.Get("Content-Type")
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = mediaType
	}
	contentLength := int64(len(body))
	if body == nil {
		contentLength = response.ContentLength
		if contentLength < 0 {
			if value := headers.Get("Content-Length"); value != "" {
				if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed >= 0 {
					contentLength = parsed
				}
			}
		}
		if contentLength < 0 {
			contentLength = 0
		}
	}
	sha := ""
	if len(body) > 0 {
		digest := sha256.Sum256(body)
		sha = hex.EncodeToString(digest[:])
	}
	now := time.Now()
	return CacheEntry{
		Version:       MetadataVersion,
		URL:           target.String(),
		Host:          target.Hostname(),
		ContentType:   contentType,
		ContentLength: contentLength,
		StatusCode:    response.StatusCode,
		Headers:       headers,
		ETag:          headers.Get("ETag"),
		LastModified:  headers.Get("Last-Modified"),
		SHA256:        sha,
		CreatedAt:     now,
		LastAccessed:  now,
	}
}

func validateResponse(response *http.Response, body []byte) error {
	if response == nil {
		return fmt.Errorf("origin returned a nil response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("status %d is not cacheable", response.StatusCode)
	}
	if len(body) == 0 {
		return fmt.Errorf("empty response body")
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(body)) {
		return fmt.Errorf("content length mismatch: header=%d body=%d", response.ContentLength, len(body))
	}
	if value := response.Header.Get("Content-Length"); value != "" {
		if length, err := strconv.ParseInt(value, 10, 64); err == nil && length != int64(len(body)) {
			return fmt.Errorf("content length mismatch: header=%d body=%d", length, len(body))
		}
	}
	if looksLikeHTML(body) {
		return fmt.Errorf("HTML error page is not a static resource")
	}
	if response.Header.Get("Content-Encoding") == "" {
		if err := validateMagic(response.Header.Get("Content-Type"), body); err != nil {
			return err
		}
	}
	return nil
}

func looksLikeHTML(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	lower := bytes.ToLower(trimmed)
	return bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) || bytes.HasPrefix(lower, []byte("<head"))
}

func validateMagic(contentType string, body []byte) error {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(mediaType)
	if mediaType == "" {
		return nil
	}
	checks := map[string]func([]byte) bool{
		"image/png": func(b []byte) bool {
			return len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
		},
		"image/jpeg": func(b []byte) bool { return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF },
		"image/gif": func(b []byte) bool {
			return len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a")))
		},
		"image/webp": func(b []byte) bool {
			return len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
		},
		"audio/mpeg": func(b []byte) bool {
			return len(b) >= 3 && (bytes.Equal(b[:3], []byte("ID3")) || (b[0] == 0xFF && b[1]&0xE0 == 0xE0))
		},
		"font/woff":  func(b []byte) bool { return len(b) >= 4 && bytes.Equal(b[:4], []byte("wOFF")) },
		"font/woff2": func(b []byte) bool { return len(b) >= 4 && bytes.Equal(b[:4], []byte("wOF2")) },
		"font/ttf": func(b []byte) bool {
			return len(b) >= 4 && (bytes.Equal(b[:4], []byte{0x00, 0x01, 0x00, 0x00}) || bytes.Equal(b[:4], []byte("true")))
		},
		"font/otf": func(b []byte) bool { return len(b) >= 4 && bytes.Equal(b[:4], []byte("OTTO")) },
	}
	if check, ok := checks[mediaType]; ok && !check(body) {
		return fmt.Errorf("content does not match Content-Type %s", mediaType)
	}
	return nil
}
