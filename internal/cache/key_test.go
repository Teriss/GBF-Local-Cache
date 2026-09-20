package cache

import (
	"net/url"
	"testing"
)

func TestCanonicalKeyIncludesHostPortAndQuery(t *testing.T) {
	first, err := url.Parse("https://Example.Test/assets/a.png?b=2&a=1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := url.Parse("https://example.test:443/assets/a.png?a=1&b=2")
	if err != nil {
		t.Fatal(err)
	}
	firstCanonical, firstHash, err := CanonicalKey(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCanonical, secondHash, err := CanonicalKey(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstCanonical == secondCanonical || firstHash == secondHash {
		t.Fatalf("query order was normalized unexpectedly: %q/%s vs %q/%s", firstCanonical, firstHash, secondCanonical, secondHash)
	}

	otherHost, _ := url.Parse("https://other.test/assets/a.png?a=1&b=2")
	_, otherHash, err := CanonicalKey(otherHost)
	if err != nil {
		t.Fatal(err)
	}
	if otherHash == firstHash {
		t.Fatal("different hosts collided")
	}

	otherQuery, _ := url.Parse("https://example.test/assets/a.png?a=2&b=1")
	_, otherQueryHash, err := CanonicalKey(otherQuery)
	if err != nil {
		t.Fatal(err)
	}
	if otherQueryHash == firstHash {
		t.Fatal("different queries collided")
	}
}
