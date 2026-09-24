package cache

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidRange       = errors.New("invalid range")
	ErrUnsatisfiableRange = errors.New("unsatisfiable range")
)

type ByteRange struct {
	Start int64
	End   int64
}

func (r ByteRange) Length() int64 {
	return r.End - r.Start + 1
}

func ParseRange(value string, size int64) (ByteRange, error) {
	if size < 0 || !strings.HasPrefix(strings.ToLower(value), "bytes=") {
		return ByteRange{}, ErrInvalidRange
	}
	spec := strings.TrimSpace(value[len("bytes="):])
	if spec == "" || strings.Contains(spec, ",") {
		return ByteRange{}, ErrInvalidRange
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return ByteRange{}, ErrInvalidRange
	}
	if size == 0 {
		return ByteRange{}, ErrUnsatisfiableRange
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || suffix <= 0 {
			return ByteRange{}, ErrInvalidRange
		}
		if suffix > size {
			suffix = size
		}
		return ByteRange{Start: size - suffix, End: size - 1}, nil
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 {
		return ByteRange{}, ErrInvalidRange
	}
	if start >= size {
		return ByteRange{}, ErrUnsatisfiableRange
	}
	if strings.TrimSpace(parts[1]) == "" {
		return ByteRange{Start: start, End: size - 1}, nil
	}
	end, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || end < start {
		return ByteRange{}, ErrInvalidRange
	}
	if end >= size {
		end = size - 1
	}
	return ByteRange{Start: start, End: end}, nil
}

func WriteResult(writer http.ResponseWriter, request *http.Request, result Result) {
	copyHeaders(writer.Header(), result.Entry.Headers)
	if result.Entry.ETag != "" {
		writer.Header().Set("ETag", result.Entry.ETag)
	}
	if result.Entry.LastModified != "" {
		writer.Header().Set("Last-Modified", result.Entry.LastModified)
	}
	if result.Entry.StatusCode == 0 {
		result.Entry.StatusCode = http.StatusOK
	}

	if result.Entry.StatusCode == http.StatusOK && isNotModified(request, result.Entry) {
		writer.Header().Del("Content-Length")
		writer.Header().Set("Content-Length", "0")
		writer.WriteHeader(http.StatusNotModified)
		return
	}

	body := result.Body
	status := result.Entry.StatusCode
	if status == http.StatusOK && len(body) > 0 {
		writer.Header().Set("Accept-Ranges", "bytes")
		if rangeHeader := request.Header.Get("Range"); rangeHeader != "" && ifRangeMatches(request, result.Entry) {
			byteRange, err := ParseRange(rangeHeader, int64(len(body)))
			if err != nil {
				writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
				writer.Header().Set("Content-Length", "0")
				writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			status = http.StatusPartialContent
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", byteRange.Start, byteRange.End, len(body)))
			writer.Header().Set("Content-Length", strconv.FormatInt(byteRange.Length(), 10))
			if request.Method != http.MethodHead {
				writer.WriteHeader(status)
				_, _ = writer.Write(body[byteRange.Start : byteRange.End+1])
				return
			}
		} else {
			writer.Header().Del("Content-Range")
			writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
	} else {
		contentLength := result.Entry.ContentLength
		if contentLength < 0 {
			contentLength = int64(len(body))
		}
		writer.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}

	writer.WriteHeader(status)
	if request.Method != http.MethodHead && status != http.StatusNotModified {
		_, _ = writer.Write(body)
	}
}

func isNotModified(request *http.Request, entry CacheEntry) bool {
	if value := request.Header.Get("If-None-Match"); value != "" {
		return etagMatches(value, entry.ETag)
	}
	if value := request.Header.Get("If-Modified-Since"); value != "" && entry.LastModified != "" {
		requested, err := http.ParseTime(value)
		modified, modifiedErr := http.ParseTime(entry.LastModified)
		return err == nil && modifiedErr == nil && !modified.After(requested.Add(time.Second))
	}
	return false
}

func ifRangeMatches(request *http.Request, entry CacheEntry) bool {
	value := request.Header.Get("If-Range")
	if value == "" {
		return true
	}
	if strings.Contains(value, "\"") {
		if strings.HasPrefix(value, "W/") || strings.HasPrefix(entry.ETag, "W/") {
			return false
		}
		return value == entry.ETag
	}
	requested, err := http.ParseTime(value)
	modified, modifiedErr := http.ParseTime(entry.LastModified)
	return err == nil && modifiedErr == nil && !modified.After(requested)
}

func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return etag != ""
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		current := strings.TrimPrefix(strings.TrimSpace(etag), "W/")
		if candidate != "" && candidate == current {
			return true
		}
	}
	return false
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		if isHopByHop(key) || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Set-Cookie") || strings.EqualFold(key, "Set-Cookie2") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
