package cache

import (
	"encoding/json"
	"net/http"
	"time"
)

const MetadataVersion = 1

type CacheEntry struct {
	Version       int         `json:"version"`
	URL           string      `json:"url"`
	Host          string      `json:"host"`
	ContentType   string      `json:"content_type"`
	ContentLength int64       `json:"content_length"`
	StatusCode    int         `json:"status_code"`
	Headers       http.Header `json:"headers,omitempty"`
	ETag          string      `json:"etag,omitempty"`
	LastModified  string      `json:"last_modified,omitempty"`
	SHA256        string      `json:"sha256"`
	CreatedAt     time.Time   `json:"created_at"`
	LastAccessed  time.Time   `json:"last_accessed"`
}

func (e CacheEntry) Clone() CacheEntry {
	copy := e
	if e.Headers != nil {
		copy.Headers = e.Headers.Clone()
	}
	return copy
}

func (e CacheEntry) Marshal() ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}

func (e *CacheEntry) Unmarshal(data []byte) error {
	return json.Unmarshal(data, e)
}
