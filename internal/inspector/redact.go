package inspector

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const redactedValue = "[redacted]"

// visibleHeaders is the small explicit non-sensitive allowlist whose values
// may be shown. Every other header value is redacted, including
// Authorization, Cookie, Set-Cookie, API keys, signed tokens and all
// Paperboat headers.
func visibleHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content-type", "content-length":
		return true
	default:
		return false
	}
}

// SanitizeHeaders returns a redacted copy of header with at most
// MaxHeadersPerDirection entries. Excess entries are dropped and reported via
// truncated. The input is never mutated.
func SanitizeHeaders(header http.Header) (http.Header, bool) {
	if len(header) == 0 {
		return http.Header{}, false
	}
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(http.Header, len(names))
	truncated := false
	kept := 0
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if kept >= MaxHeadersPerDirection {
			truncated = true
			break
		}
		values := header.Values(name)
		if visibleHeader(name) {
			copied := make([]string, len(values))
			copy(copied, values)
			out[canonicalHeaderKey(name)] = copied
		} else {
			masked := make([]string, len(values))
			for i := range values {
				masked[i] = redactedValue
			}
			if len(masked) == 0 {
				masked = []string{redactedValue}
			}
			out[canonicalHeaderKey(name)] = masked
		}
		kept++
	}
	return out, truncated
}

func canonicalHeaderKey(name string) string {
	return http.CanonicalHeaderKey(strings.TrimSpace(name))
}

// SanitizeURL returns a sanitized URL string with userinfo removed and every
// query value redacted. The path, host and query keys are preserved so the
// record remains useful for debugging without exposing secrets. URLs longer
// than URLMaxBytes are truncated and reported.
func SanitizeURL(raw string) (string, bool) {
	if len(raw) > URLInputMaxBytes {
		return redactedValue, true
	}
	inputExceededLimit := len(raw) > URLMaxBytes
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		// Fail closed: never echo an unparseable URL with possible secrets.
		return redactedValue, true
	}
	parsed.User = nil
	if parsed.RawQuery != "" {
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			parsed.RawQuery = "redacted"
		} else {
			for key := range query {
				query[key] = maskedStrings(len(query[key]))
			}
			parsed.RawQuery = query.Encode()
		}
	}
	sanitized := parsed.String()
	if sanitized == "" {
		return redactedValue, true
	}
	if len(sanitized) > URLMaxBytes {
		return sanitized[:URLMaxBytes], true
	}
	return sanitized, inputExceededLimit
}

func maskedStrings(count int) []string {
	if count < 1 {
		return []string{redactedValue}
	}
	out := make([]string, count)
	for i := range out {
		out[i] = redactedValue
	}
	return out
}

// sensitiveBodyField reports whether a JSON field name must be redacted: the
// built-in case-insensitive password/secret/token/key matcher plus configured
// additional sensitive names.
func sensitiveBodyField(name string, extra []string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "token") || strings.Contains(lower, "key") {
		return true
	}
	for _, candidate := range extra {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

// SanitizeJSONBody redacts sensitive fields recursively in a bounded UTF-8
// JSON document. It fails closed on malformed input, non-object/array roots
// handled as values, or unsupported encoding: callers must then mark the body
// unsupported and store nothing.
func SanitizeJSONBody(body []byte, extra []string) ([]byte, error) {
	if len(body) == 0 {
		return []byte{}, nil
	}
	if !json.Valid(body) {
		return nil, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	out := &boundedJSONBuffer{}
	if err := writeSanitizedJSON(decoder, out, extra, 0); err != nil {
		return nil, ErrInvalid
	}
	if decoder.More() {
		return nil, ErrInvalid
	}
	return out.bytes, nil
}

type boundedJSONBuffer struct{ bytes []byte }

func (b *boundedJSONBuffer) write(payload []byte) error {
	if int64(len(b.bytes)+len(payload)) > BodyMaxBytes {
		return ErrInvalid
	}
	needed := len(b.bytes) + len(payload)
	if needed <= cap(b.bytes) {
		b.bytes = append(b.bytes, payload...)
		return nil
	}
	bounded := make([]byte, needed)
	copy(bounded, b.bytes)
	copy(bounded[len(b.bytes):], payload)
	b.bytes = bounded
	return nil
}

func writeSanitizedJSON(decoder *json.Decoder, out *boundedJSONBuffer, extra []string, depth int) error {
	if depth > JSONMaxDepth {
		return ErrInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		encoded, err := json.Marshal(token)
		if err != nil {
			return err
		}
		return out.write(encoded)
	}
	switch delimiter {
	case '{':
		if err := out.write([]byte{'{'}); err != nil {
			return err
		}
		first := true
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalid
			}
			if !first {
				if err := out.write([]byte{','}); err != nil {
					return err
				}
			}
			first = false
			encodedKey, _ := json.Marshal(key)
			if err := out.write(encodedKey); err != nil {
				return err
			}
			if err := out.write([]byte{':'}); err != nil {
				return err
			}
			if sensitiveBodyField(key, extra) {
				if err := discardJSONValue(decoder, depth+1); err != nil {
					return err
				}
				encodedRedacted, _ := json.Marshal(redactedValue)
				if err := out.write(encodedRedacted); err != nil {
					return err
				}
			} else if err := writeSanitizedJSON(decoder, out, extra, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalid
		}
		return out.write([]byte{'}'})
	case '[':
		if err := out.write([]byte{'['}); err != nil {
			return err
		}
		first := true
		for decoder.More() {
			if !first {
				if err := out.write([]byte{','}); err != nil {
					return err
				}
			}
			first = false
			if err := writeSanitizedJSON(decoder, out, extra, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalid
		}
		return out.write([]byte{']'})
	default:
		return ErrInvalid
	}
}

func discardJSONValue(decoder *json.Decoder, depth int) error {
	if depth > JSONMaxDepth {
		return ErrInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrInvalid
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := discardJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// jsonBodySupported reports whether a body is eligible for redacted JSON
// display: bounded, valid UTF-8 JSON with a JSON content type.
func jsonBodySupported(contentType string, body []byte) bool {
	if int64(len(body)) > BodyMaxBytes {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(contentType))
	if !strings.Contains(lower, "application/json") && !strings.Contains(lower, "+json") {
		return false
	}
	for i := 0; i < len(body); {
		c := body[i]
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
		i++
	}
	return json.Valid(body)
}
