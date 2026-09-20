package logging

import "testing"

func TestRingKeepsNewestEntries(t *testing.T) {
	ring := NewRing(2)
	ring.Add(Entry{Message: "one"})
	ring.Add(Entry{Message: "two"})
	ring.Add(Entry{Message: "three"})
	entries := ring.Snapshot()
	if len(entries) != 2 || entries[0].Message != "two" || entries[1].Message != "three" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestRingRecentLimitsNewestEntries(t *testing.T) {
	ring := NewRing(5)
	for _, message := range []string{"one", "two", "three", "four"} {
		ring.Add(Entry{Message: message})
	}
	entries := ring.Recent(2)
	if len(entries) != 2 || entries[0].Message != "three" || entries[1].Message != "four" {
		t.Fatalf("recent entries = %#v", entries)
	}
}
