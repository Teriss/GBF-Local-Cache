package cache

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRangeForms(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  ByteRange
		err   error
	}{
		{name: "start-end", value: "bytes=0-99", want: ByteRange{Start: 0, End: 99}},
		{name: "open-ended", value: "bytes=100-", want: ByteRange{Start: 100, End: 199}},
		{name: "suffix", value: "bytes=-100", want: ByteRange{Start: 100, End: 199}},
		{name: "clamp-end", value: "bytes=150-999", want: ByteRange{Start: 150, End: 199}},
		{name: "multiple", value: "bytes=0-1,3-4", err: ErrInvalidRange},
		{name: "outside", value: "bytes=200-", err: ErrUnsatisfiableRange},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseRange(test.value, 200)
			if test.err != nil {
				if err != test.err {
					t.Fatalf("error = %v, want %v", err, test.err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("range = %#v, error = %v, want %#v", got, err, test.want)
			}
		})
	}
}

func TestWriteResultSupportsValidatorsAndRange(t *testing.T) {
	entry := CacheEntry{
		StatusCode:   http.StatusOK,
		Headers:      http.Header{"Content-Type": []string{"audio/mpeg"}},
		ETag:         `"asset-v1"`,
		LastModified: "Wed, 01 Jan 2025 00:00:00 GMT",
	}
	entry.Headers.Set("ETag", entry.ETag)
	entry.Headers.Set("Last-Modified", entry.LastModified)
	result := Result{Entry: entry, Body: []byte(strings.Repeat("a", 200)), Cacheable: true}

	rangeRequest := httptest.NewRequest(http.MethodGet, "https://static.example.test/voice.mp3", nil)
	rangeRequest.Header.Set("Range", "bytes=100-199")
	rangeResponse := httptest.NewRecorder()
	WriteResult(rangeResponse, rangeRequest, result)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.Len() != 100 || rangeResponse.Header().Get("Content-Range") != "bytes 100-199/200" {
		t.Fatalf("range response = status %d, length %d, range %q", rangeResponse.Code, rangeResponse.Body.Len(), rangeResponse.Header().Get("Content-Range"))
	}

	notModifiedRequest := httptest.NewRequest(http.MethodGet, "https://static.example.test/voice.mp3", nil)
	notModifiedRequest.Header.Set("If-None-Match", `W/"asset-v1"`)
	notModifiedResponse := httptest.NewRecorder()
	WriteResult(notModifiedResponse, notModifiedRequest, result)
	if notModifiedResponse.Code != http.StatusNotModified || notModifiedResponse.Body.Len() != 0 {
		t.Fatalf("304 response = status %d, body length %d", notModifiedResponse.Code, notModifiedResponse.Body.Len())
	}
}
