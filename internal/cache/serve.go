package cache

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gbf-local-cache/internal/logging"
)

// ServeTiming measures the cold path from the start of cache serving. A zero
// origin time means the response was served from a cache tier.
type ServeTiming struct {
	OriginAttempted  bool
	OriginHeaders    time.Duration
	OriginFirstByte  time.Duration
	DownloadDone     time.Duration
	BrowserFirstByte time.Duration
}

type ServeOutcome struct {
	Result             Result
	Bytes              int64
	Committed          bool
	ClientDisconnected bool
	Timing             ServeTiming
	FailureStage       string
}

// A canceled browser should not force a small, otherwise useful asset to be
// downloaded again on the next visit. Bound the detached work independently
// of the general cache object limit.
const disconnectedFillLimit int64 = 8 << 20

// Serve writes cached results normally and streams ordinary cold GETs. Range
// and HEAD retain the buffered path, which needs the complete representation
// to calculate response headers.
func (m *Manager) Serve(writer http.ResponseWriter, request *http.Request) (ServeOutcome, error) {
	started := time.Now()
	if request == nil || request.URL == nil {
		return ServeOutcome{}, errors.New("cache request is missing")
	}
	if request.Method != http.MethodGet || request.Header.Get("Range") != "" {
		result, err := m.Fetch(request.Context(), request)
		if err != nil {
			return ServeOutcome{}, err
		}
		WriteResult(writer, request, result)
		return ServeOutcome{Result: result, Bytes: servedBytes(request, result.Entry, result.Body), Committed: true,
			Timing: ServeTiming{BrowserFirstByte: time.Since(started)}}, nil
	}
	canonical, hash, err := CanonicalKey(request.URL)
	if err != nil {
		return ServeOutcome{}, err
	}
	if cached, ok, lookupErr := m.lookup(request.Context(), request, hash, canonical); ok || lookupErr != nil {
		if lookupErr != nil {
			return ServeOutcome{}, lookupErr
		}
		WriteResult(writer, request, cached)
		return ServeOutcome{Result: cached, Bytes: servedBytes(request, cached.Entry, cached.Body), Committed: true,
			Timing: ServeTiming{BrowserFirstByte: time.Since(started)}}, nil
	}

	// Only the singleflight leader writes to its browser. Followers receive the
	// completed representation and write it after the shared fetch finishes.
	// Do runs the leader closure in its handler goroutine, so the ResponseWriter
	// is never used after that handler returns.
	var leaderOutcome ServeOutcome
	value, fetchErr, _ := m.flight.Do(hash, func() (any, error) {
		if entry, body, ok := m.ram.Get(hash); ok {
			return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceRAM, Cacheable: true}, nil
		}
		if entry, body, _, diskErr := m.disk.Read(hash); diskErr == nil {
			m.ram.Put(hash, entry, body)
			return Result{Key: hash, Canonical: canonical, Entry: entry, Body: body, Source: SourceDisk, Cacheable: true}, nil
		}
		var streamErr error
		leaderOutcome, streamErr = m.streamOrigin(writer, request, hash, canonical, started)
		return leaderOutcome.Result, streamErr
	})
	if fetchErr != nil {
		return leaderOutcome, fetchErr
	}
	result, ok := value.(Result)
	if !ok {
		return leaderOutcome, errors.New("invalid singleflight result")
	}
	if leaderOutcome.Committed {
		return leaderOutcome, nil
	}
	WriteResult(writer, request, result)
	return ServeOutcome{Result: result, Bytes: servedBytes(request, result.Entry, result.Body), Committed: true,
		Timing: ServeTiming{BrowserFirstByte: time.Since(started)}}, nil
}

func (m *Manager) streamOrigin(writer http.ResponseWriter, request *http.Request, hash, canonical string, started time.Time) (ServeOutcome, error) {
	outcome := ServeOutcome{Timing: ServeTiming{OriginAttempted: true}}
	m.stats.Miss()
	response, err := m.origin.Do(m.lifecycle, m.originRequest(m.lifecycle, request, nil))
	outcome.Timing.OriginHeaders = time.Since(started)
	if err != nil {
		m.stats.Error()
		outcome.FailureStage = "origin_headers"
		return outcome, err
	}
	if response == nil || response.Body == nil {
		m.stats.Error()
		outcome.FailureStage = "origin_headers"
		return outcome, errors.New("origin returned an empty response")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		result, consumeErr := m.consumeOriginResponse(request, hash, canonical, response)
		outcome.Result = result
		outcome.Timing.DownloadDone = time.Since(started)
		return outcome, consumeErr
	}
	response.Body = &firstByteBody{ReadCloser: response.Body, started: started, first: &outcome.Timing.OriginFirstByte}
	if err := decodeOriginResponse(response); err != nil {
		m.stats.Error()
		outcome.FailureStage = "decode"
		return outcome, err
	}
	if response.ContentLength > m.maxObjectSize {
		m.stats.Error()
		outcome.FailureStage = "size_limit"
		return outcome, fmt.Errorf("origin object is larger than %d bytes", m.maxObjectSize)
	}

	// Magic-byte checks need at most twelve bytes. Sixteen also catches normal
	// HTML error pages before any bytes are committed to the browser.
	var prefix [16]byte
	prefixSize, prefixErr := io.ReadFull(response.Body, prefix[:])
	if prefixErr != nil && !errors.Is(prefixErr, io.EOF) && !errors.Is(prefixErr, io.ErrUnexpectedEOF) {
		m.stats.Error()
		outcome.FailureStage = "origin_body"
		return outcome, prefixErr
	}
	if prefixSize == 0 {
		m.stats.Error()
		outcome.FailureStage = "origin_body"
		return outcome, errors.New("origin returned an empty response body")
	}
	if err := validatePrefix(response, prefix[:prefixSize]); err != nil {
		m.stats.Error()
		outcome.FailureStage = "validation"
		m.log(logging.CategoryError, request, err.Error(), prefixSize)
		return outcome, err
	}

	copyHeaders(writer.Header(), response.Header)
	if response.ContentLength >= 0 {
		writer.Header().Set("Content-Length", fmt.Sprintf("%d", response.ContentLength))
	}
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.WriteHeader(http.StatusOK)
	outcome.Committed = true
	var body bytes.Buffer
	body.Grow(prefixSize)
	_, _ = body.Write(prefix[:prefixSize])
	count := int64(prefixSize)
	storeAllowed := CanStoreResponse(response.Header) && !RequestDisallowsStore(request)
	var clientWriteErr error
	if err := writeStreamChunk(writer, prefix[:prefixSize]); err != nil {
		outcome.ClientDisconnected = true
		clientWriteErr = err
		if !storeAllowed || response.ContentLength > disconnectedFillLimit {
			return outcome, err
		}
	} else {
		flushResponse(writer)
		outcome.Timing.BrowserFirstByte = time.Since(started)
	}

	buffer := make([]byte, 32*1024)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			count += int64(n)
			if count > m.maxObjectSize {
				m.stats.Error()
				outcome.FailureStage = "size_limit"
				return outcome, fmt.Errorf("origin object is larger than %d bytes", m.maxObjectSize)
			}
			_, _ = body.Write(buffer[:n])
			if !outcome.ClientDisconnected {
				if err := writeStreamChunk(writer, buffer[:n]); err != nil {
					outcome.ClientDisconnected = true
					clientWriteErr = err
					if !storeAllowed || response.ContentLength > disconnectedFillLimit {
						return outcome, err
					}
				} else {
					flushResponse(writer)
				}
			}
			if outcome.ClientDisconnected && count > disconnectedFillLimit {
				return outcome, clientWriteErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			m.stats.Error()
			outcome.FailureStage = "origin_body"
			return outcome, readErr
		}
	}
	outcome.Timing.DownloadDone = time.Since(started)
	m.stats.AddOriginBytes(count)
	data := body.Bytes()
	if err := validateResponse(response, data); err != nil {
		m.stats.Error()
		outcome.FailureStage = "validation"
		m.log(logging.CategoryError, request, "validation failed: "+err.Error(), len(data))
		return outcome, err
	}
	entry := newEntry(request.URL, response, data)
	entry.Headers.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	entry.ContentLength = int64(len(data))
	if storeAllowed {
		m.store(hash, entry, data, request)
	}
	message := "origin"
	if !storeAllowed {
		message = "origin (not stored)"
	}
	m.log(logging.CategoryMiss, request, message, len(data))
	outcome.Result = Result{Key: hash, Canonical: canonical, Entry: entry, Body: data, Source: SourceOrigin, Cacheable: storeAllowed}
	outcome.Bytes = count
	return outcome, nil
}

func flushResponse(writer http.ResponseWriter) {
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeStreamChunk(writer http.ResponseWriter, chunk []byte) error {
	n, err := writer.Write(chunk)
	if err != nil {
		return err
	}
	if n != len(chunk) {
		return io.ErrShortWrite
	}
	return nil
}

func validatePrefix(response *http.Response, prefix []byte) error {
	if looksLikeHTML(prefix) {
		return errors.New("HTML error page is not a static resource")
	}
	if IsStaticContentType(response.Header.Get("Content-Type")) {
		if err := validateMagic(response.Header.Get("Content-Type"), prefix); err != nil {
			return err
		}
	}
	return nil
}

type firstByteBody struct {
	io.ReadCloser
	started time.Time
	first   *time.Duration
}

func (body *firstByteBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if n > 0 && *body.first == 0 {
		*body.first = time.Since(body.started)
	}
	return n, err
}

type gzipBody struct {
	*gzip.Reader
	raw io.ReadCloser
}

func (body *gzipBody) Close() error {
	readerErr := body.Reader.Close()
	rawErr := body.raw.Close()
	if readerErr != nil {
		return readerErr
	}
	return rawErr
}

func decodeOriginResponse(response *http.Response) error {
	if response.Uncompressed {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return nil
	}
	if encoding != "gzip" {
		return fmt.Errorf("unsupported origin content encoding %q", encoding)
	}
	raw := response.Body
	reader, err := gzip.NewReader(raw)
	if err != nil {
		return fmt.Errorf("decode gzip origin response: %w", err)
	}
	response.Body = &gzipBody{Reader: reader, raw: raw}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.Header.Del("Content-Md5")
	if etag := response.Header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		response.Header.Set("ETag", "W/"+etag)
	}
	response.ContentLength = -1
	response.Uncompressed = true
	return nil
}
