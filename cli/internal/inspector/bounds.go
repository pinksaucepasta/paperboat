package inspector

import (
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"
)

// Captures show requests as they were sent, like ngrok's inspector (user
// decision 2026-10-23): only people allowed to inspect a preview read them,
// so headers, query strings and bodies are kept in full. This file only
// bounds what is stored.

const truncatedURL = "[url too long]"

// SanitizeHeaders returns a copy of header with at most
// MaxHeadersPerDirection entries, values in full. Excess entries are dropped
// and reported via truncated. The input is never mutated.
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
		copied := make([]string, len(values))
		copy(copied, values)
		out[canonicalHeaderKey(name)] = copied
		kept++
	}
	return out, truncated
}

func canonicalHeaderKey(name string) string {
	return http.CanonicalHeaderKey(strings.TrimSpace(name))
}

// SanitizeURL returns the request URL as sent, cut to URLMaxBytes on a UTF-8
// boundary. Inputs over URLInputMaxBytes are not stored at all.
func SanitizeURL(raw string) (string, bool) {
	if raw == "" || len(raw) > URLInputMaxBytes {
		return truncatedURL, true
	}
	if len(raw) <= URLMaxBytes {
		return raw, false
	}
	return string(textPrefix([]byte(raw), URLMaxBytes)), true
}

// textBody reports whether body can be shown as text: valid UTF-8 without
// control characters other than tab, newline and carriage return. Binary
// bodies are marked unsupported instead.
func textBody(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	for _, c := range body {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// textPrefix cuts body to at most limit bytes without splitting a UTF-8
// sequence.
func textPrefix(body []byte, limit int) []byte {
	if len(body) <= limit {
		return body
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut]
}
