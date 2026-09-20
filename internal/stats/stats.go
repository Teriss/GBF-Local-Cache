package stats

import "sync/atomic"

type Counters struct {
	TotalRequests     uint64 `json:"total_requests"`
	RamHits           uint64 `json:"ram_hits"`
	DiskHits          uint64 `json:"disk_hits"`
	Misses            uint64 `json:"misses"`
	OriginBytes       uint64 `json:"origin_bytes"`
	CacheBytesServed  uint64 `json:"cache_bytes_served"`
	BytesSaved        uint64 `json:"bytes_saved"`
	Errors            uint64 `json:"errors"`
	RangeRequests     uint64 `json:"range_requests"`
	CacheableRequests uint64 `json:"cacheable_requests"`
}

type Stats struct {
	totalRequests     atomic.Uint64
	ramHits           atomic.Uint64
	diskHits          atomic.Uint64
	misses            atomic.Uint64
	originBytes       atomic.Uint64
	cacheBytesServed  atomic.Uint64
	bytesSaved        atomic.Uint64
	errors            atomic.Uint64
	rangeRequests     atomic.Uint64
	cacheableRequests atomic.Uint64
}

func (s *Stats) Request() {
	s.totalRequests.Add(1)
}

func (s *Stats) CacheableRequest() {
	s.cacheableRequests.Add(1)
}

func (s *Stats) RamHit() {
	s.ramHits.Add(1)
}

func (s *Stats) DiskHit() {
	s.diskHits.Add(1)
}

func (s *Stats) Miss() {
	s.misses.Add(1)
}

func (s *Stats) AddOriginBytes(n int64) {
	if n > 0 {
		s.originBytes.Add(uint64(n))
	}
}

func (s *Stats) AddCacheBytesServed(n int64) {
	if n > 0 {
		s.cacheBytesServed.Add(uint64(n))
	}
}

func (s *Stats) AddBytesSaved(n int64) {
	if n > 0 {
		s.bytesSaved.Add(uint64(n))
	}
}

func (s *Stats) Error() {
	s.errors.Add(1)
}

func (s *Stats) RangeRequest() {
	s.rangeRequests.Add(1)
}

func (s *Stats) Snapshot() Counters {
	return Counters{
		TotalRequests:     s.totalRequests.Load(),
		RamHits:           s.ramHits.Load(),
		DiskHits:          s.diskHits.Load(),
		Misses:            s.misses.Load(),
		OriginBytes:       s.originBytes.Load(),
		CacheBytesServed:  s.cacheBytesServed.Load(),
		BytesSaved:        s.bytesSaved.Load(),
		Errors:            s.errors.Load(),
		RangeRequests:     s.rangeRequests.Load(),
		CacheableRequests: s.cacheableRequests.Load(),
	}
}
