package inspector

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// BodyTap copies at most BodyMaxBytes from a stream without blocking or
// buffering the full body. It never returns a tap error and never retains a
// buffer after the caller releases it.
type BodyTap struct {
	mu        sync.Mutex
	buffer    []byte
	truncated bool
	limit     int64
	disabled  bool
}

func newBodyTap(limit int64, enabled bool) *BodyTap {
	return &BodyTap{limit: limit, disabled: !enabled}
}

func newPrefixedBodyTap(limit int64, enabled bool, prefix []byte) *BodyTap {
	tap := newBodyTap(limit, enabled)
	if enabled {
		_, _ = tap.Write(prefix)
	}
	return tap
}

func (t *BodyTap) Write(payload []byte) (int, error) {
	if t == nil {
		return len(payload), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.disabled {
		return len(payload), nil
	}
	limit := t.limit
	if limit == 0 {
		limit = BodyMaxBytes
	}
	remaining := limit - int64(len(t.buffer))
	if remaining <= 0 {
		t.truncated = true
		return len(payload), nil
	}
	if int64(len(payload)) > remaining {
		t.appendBounded(payload[:remaining])
		t.truncated = true
		return len(payload), nil
	}
	t.appendBounded(payload)
	return len(payload), nil
}

func (t *BodyTap) appendBounded(payload []byte) {
	needed := len(t.buffer) + len(payload)
	if needed <= cap(t.buffer) {
		t.buffer = append(t.buffer, payload...)
		return
	}
	bounded := make([]byte, needed)
	copy(bounded, t.buffer)
	copy(bounded[len(t.buffer):], payload)
	t.buffer = bounded
}

// TapReader returns a reader that forwards every byte while tapping a bounded
// prefix for capture. Forwarding never waits for capture.
func (t *BodyTap) TapReader(reader io.Reader) io.Reader {
	if t == nil || reader == nil {
		return reader
	}
	return io.TeeReader(reader, t)
}

func (t *BodyTap) Bytes() []byte {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buffer...)
}

func (t *BodyTap) Truncated() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.truncated
}

// Take transfers the retained prefix and releases the tap buffer.
func (t *BodyTap) Take() ([]byte, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	buffer, truncated := t.buffer, t.truncated
	t.buffer = nil
	t.disabled = true
	return buffer, truncated
}

func (t *BodyTap) release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.buffer = nil
	t.disabled = true
	t.mu.Unlock()
}

// RequestBodyTap, ResponseBodyTap, and RawBodyTap create bounded taps owned by
// this admitted capture. Disabled policy modes retain no bytes. RawBodyTap's
// limit is the whole raw-request ceiling; callers must still include serialized
// request headers in that same ceiling when assembling RawRequest.
func (p *Pending) RequestBodyTap() *BodyTap {
	return p.registerTap(newBodyTap(BodyMaxBytes, p != nil && p.policy.CaptureRequestBody), false)
}

func (p *Pending) ResponseBodyTap() *BodyTap {
	return p.registerTap(newBodyTap(BodyMaxBytes, p != nil && p.policy.CaptureResponseBody), false)
}

func (p *Pending) RawRequestTap(prefix []byte) *BodyTap {
	return p.registerTap(newPrefixedBodyTap(RawMaxBytes, p != nil && p.policy.CaptureRaw, prefix), true)
}

func (p *Pending) RawEnabled() bool { return p != nil && p.policy.CaptureRaw }

// headerBytes estimates sanitized header storage.
func headerBytes(header http.Header) int64 {
	var total int64
	for name, values := range header {
		total += int64(len(name))
		for _, value := range values {
			total += int64(len(value))
		}
	}
	return total
}

func errorCodeOrNone(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "none"
	}
	if len(code) > 128 {
		return code[:128]
	}
	return code
}

func buildRecord(pending *Pending, policy ResourcePolicy, completed CompletedCapture, now time.Time) *Record {
	started := completed.StartedAt.UTC()
	if started.IsZero() {
		started = pending.StartedAt.UTC()
	}
	finished := completed.FinishedAt.UTC()
	if finished.IsZero() || finished.Before(started) {
		finished = now.UTC()
	}
	method := strings.ToUpper(strings.TrimSpace(completed.Method))
	if method == "" {
		method = "GET"
	}
	if len(method) > 16 {
		method = method[:16]
	}
	sanitizedURL, urlTruncated := SanitizeURL(strings.TrimSpace(completed.RawURL))
	if strings.TrimSpace(completed.RawURL) == "" {
		sanitizedURL, urlTruncated = redactedValue, true
	}
	requestHeaders, requestHeaderTruncated := SanitizeHeaders(completed.RequestHeaders)
	responseHeaders, responseHeaderTruncated := SanitizeHeaders(completed.ResponseHeaders)
	headersTruncated := requestHeaderTruncated || responseHeaderTruncated

	requestBody, requestState := sanitizeBody(
		completed.RequestBody, completed.RequestTruncated, completed.RequestUnsupported,
		policy.CaptureRequestBody, requestHeaders.Get("Content-Type"), policy.SensitiveNames)
	responseBody, responseState := sanitizeBody(
		completed.ResponseBody, completed.ResponseTruncated, completed.ResponseUnsupported,
		policy.CaptureResponseBody, responseHeaders.Get("Content-Type"), policy.SensitiveNames)

	// Enforce the 16 KiB metadata cap by dropping response headers first,
	// then request headers, keeping method/URL/status always.
	metadataSize := int64(len(sanitizedURL)+len(method)+512) + headerBytes(requestHeaders) + headerBytes(responseHeaders)
	for metadataSize > MetadataMaxBytes && len(responseHeaders) > 0 {
		dropOneHeader(responseHeaders)
		headersTruncated = true
		metadataSize = int64(len(sanitizedURL)+len(method)+512) + headerBytes(requestHeaders) + headerBytes(responseHeaders)
	}
	for metadataSize > MetadataMaxBytes && len(requestHeaders) > 0 {
		dropOneHeader(requestHeaders)
		headersTruncated = true
		metadataSize = int64(len(sanitizedURL)+len(method)+512) + headerBytes(requestHeaders) + headerBytes(responseHeaders)
	}
	if metadataSize > MetadataMaxBytes && len(sanitizedURL) > 256 {
		sanitizedURL = sanitizedURL[:256]
		urlTruncated = true
		metadataSize = int64(len(sanitizedURL)+len(method)+512) + headerBytes(requestHeaders) + headerBytes(responseHeaders)
	}

	state := StateComplete
	if requestState == StateDropped || responseState == StateDropped {
		state = StateDropped
	}
	if requestState == StateUnsupported || responseState == StateUnsupported {
		state = StateUnsupported
	}
	if requestState == StateTruncated || responseState == StateTruncated {
		state = StateTruncated
	}

	replayIneligible := ""
	switch {
	case !policy.CaptureRaw:
		replayIneligible = "raw_disabled"
	case completed.RequestIncomplete:
		replayIneligible = "request_incomplete"
	case completed.RequestUnsupported || completed.ResponseUnsupported:
		replayIneligible = "stream_unsupported"
	case completed.RawTruncated:
		replayIneligible = "raw_truncated"
	case len(completed.RawRequest) == 0:
		replayIneligible = "raw_missing"
	case int64(len(completed.RawRequest)) > RawMaxBytes:
		replayIneligible = "raw_too_large"
	case !now.Before(started.Add(RawRetention)):
		replayIneligible = "raw_expired"
	}

	estimated := metadataSize + int64(len(requestBody)) + int64(len(responseBody))
	if replayIneligible == "" {
		estimated += int64(len(completed.RawRequest))
	}
	record := &Record{
		ID:                 pending.ID,
		ResourceID:         pending.ResourceID,
		ResourceGeneration: completed.ResourceGeneration,
		RouteGeneration:    completed.RouteGeneration,
		TargetGeneration:   completed.TargetGeneration,
		Method:             method,
		URL:                sanitizedURL,
		URLTruncated:       urlTruncated,
		RequestHeaders:     requestHeaders,
		ResponseHeaders:    responseHeaders,
		HeadersTruncated:   headersTruncated,
		RequestBody:        requestBody,
		RequestBodyState:   requestState,
		ResponseBody:       responseBody,
		ResponseBodyState:  responseState,
		ResponseStatus:     completed.ResponseStatus,
		ErrorCode:          errorCodeOrNone(completed.ErrorCode),
		State:              state,
		ReplayIneligible:   replayIneligible,
		StartedAt:          started,
		FinishedAt:         finished,
		EstimatedBytes:     estimated,
	}
	if record.ResourceGeneration == 0 {
		record.ResourceGeneration = pending.ResourceGeneration
	}
	if record.RouteGeneration == 0 {
		record.RouteGeneration = pending.RouteGeneration
	}
	if record.TargetGeneration == 0 {
		record.TargetGeneration = pending.TargetGeneration
	}
	return record
}

func dropOneHeader(header http.Header) {
	for name := range header {
		delete(header, name)
		return
	}
}

// sanitizeBody applies the opt-in policy: disabled modes report dropped
// without storing bytes; unsupported streams and non-JSON bodies report
// unsupported; truncated captures keep their state; otherwise bodies are
// redacted as bounded UTF-8 JSON or marked unsupported when redaction fails
// closed.
func sanitizeBody(body []byte, truncated, unsupported, enabled bool, contentType string, sensitive []string) ([]byte, CaptureState) {
	if unsupported {
		return nil, StateUnsupported
	}
	if !enabled {
		return nil, StateDropped
	}
	if truncated && int64(len(body)) >= BodyMaxBytes {
		// Attempt to redact the retained prefix; a cut mid-value fails
		// closed to unsupported rather than leaking a partial secret.
		if !jsonBodySupported(contentType, body) {
			return nil, StateTruncated
		}
		sanitized, err := SanitizeJSONBody(body, sensitive)
		if err != nil {
			return nil, StateTruncated
		}
		return sanitized, StateTruncated
	}
	if len(body) == 0 {
		return nil, StateComplete
	}
	if !jsonBodySupported(contentType, body) {
		return nil, StateUnsupported
	}
	sanitized, err := SanitizeJSONBody(body, sensitive)
	if err != nil {
		return nil, StateUnsupported
	}
	return sanitized, StateComplete
}
