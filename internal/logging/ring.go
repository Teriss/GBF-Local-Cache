package logging

import (
	"strings"
	"sync"
	"time"
)

type Category string

const (
	CategoryHit       Category = "HIT"
	CategoryMiss      Category = "MISS"
	CategoryStore     Category = "STORE"
	CategoryRange     Category = "RANGE"
	CategoryError     Category = "ERROR"
	CategoryNetwork   Category = "NETWORK"
	CategoryMigration Category = "MIGRATION"
	CategoryCert      Category = "CERT"
)

type Entry struct {
	Time     time.Time `json:"time"`
	Category Category  `json:"category"`
	Method   string    `json:"method,omitempty"`
	Target   string    `json:"target,omitempty"`
	Message  string    `json:"message,omitempty"`
	Duration string    `json:"duration,omitempty"`
}

type Ring struct {
	mu      sync.RWMutex
	entries []Entry
	next    int
	full    bool
}

func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 5000
	}
	return &Ring{entries: make([]Entry, capacity)}
}

func (r *Ring) Add(entry Entry) {
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	entry.Method = strings.ToUpper(entry.Method)

	r.mu.Lock()
	r.entries[r.next] = entry
	r.next = (r.next + 1) % len(r.entries)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

func (r *Ring) Snapshot() []Entry {
	return r.Recent(0)
}

// Recent returns the newest limit entries in chronological order. A non-
// positive limit returns the complete ring. Keeping the limit in the backend
// prevents every UI snapshot from serializing the full ring buffer.
func (r *Ring) Recent(limit int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := r.next
	start := 0
	if r.full {
		count = len(r.entries)
		start = r.next
	}
	if limit > 0 && count > limit {
		start = (start + count - limit) % len(r.entries)
		count = limit
	}
	result := make([]Entry, 0, count)
	for i := 0; i < count; i++ {
		result = append(result, r.entries[(start+i)%len(r.entries)])
	}
	return result
}

// Clear drops the in-memory history without affecting the running service.
// It is useful for the GUI log page and intentionally does not touch any
// optional disk log (disk logging remains disabled by default).
func (r *Ring) Clear() {
	r.mu.Lock()
	for index := range r.entries {
		r.entries[index] = Entry{}
	}
	r.next = 0
	r.full = false
	r.mu.Unlock()
}
