package stats

import (
	"sync"
	"testing"
)

func TestStatsAreSafeForConcurrentUpdates(t *testing.T) {
	var s Stats
	const workers = 20
	const updates = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < updates; j++ {
				s.Request()
				s.CacheableRequest()
				s.AddOriginBytes(3)
			}
		}()
	}
	wg.Wait()
	got := s.Snapshot()
	if got.TotalRequests != workers*updates || got.CacheableRequests != workers*updates || got.OriginBytes != workers*updates*3 {
		t.Fatalf("unexpected counters: %#v", got)
	}
}
