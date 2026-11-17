package safe

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/shared/redact"
)

const (
	// RedactedValue is the only replacement used for secret-like values.
	RedactedValue = "[REDACTED]"

	// Field bounds shared by health summaries, repair actions and event messages.
	MaximumSummaryBytes = 256
	MaximumRepairBytes  = 256
	MaximumMessageBytes = 512
)

var secretStringRedactions = []redact.Rule{
	{Pattern: regexp.MustCompile(`(?is)-----BEGIN (?:[A-Z0-9 ]* )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]* )?PRIVATE KEY-----`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?im)^\s*(?:authorization|proxy-authorization|cookie|set-cookie)\s*:\s*.*$`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)("(?:authorization|proxy_authorization|cookie|set_cookie|token|access_token|refresh_token|password|passwd|secret|api_key|client_secret|private_key|credential|credential_ref|credential_reference|secret_ref|token_ref|signed_url)"\s*:\s*)"[^"]*"`), Replacement: `${1}"` + RedactedValue + `"`},
	{Pattern: regexp.MustCompile(`(?i)((?:authorization|proxy[_-]?authorization|cookie|set[_-]?cookie|token|access[_-]?token|refresh[_-]?token|password|passwd|secret|api[_-]?key|client[_-]?secret|private[_-]?key|credential(?:[_-]?(?:ref|reference))?|secret[_-]?ref|token[_-]?ref|signed[_-]?url)\s*[=:]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`), Replacement: `${1}` + RedactedValue},
	{Pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9]{20,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{16,}\b`), Replacement: RedactedValue},
}

var safeStringRedactions = append(append([]redact.Rule(nil), secretStringRedactions...), []redact.Rule{
	{Pattern: regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s]+`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)(?:/Users/|/home/|[A-Z]:\\Users\\)[^\s,;]+`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\b(?:account|actor|assignment|certificate|connector|correlation|machine|domain|edge|host|operation|process|request|resource|route|session|tunnel)_[A-Za-z0-9_.:-]+\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+[a-z]{2,}\b`), Replacement: RedactedValue},
}...)

// Redact returns a bounded, printable, secret-safe form of value. It never
// returns an error, so it is safe for last-resort logging paths.
func Redact(value string) string {
	redacted, err := BoundedString(value, MaximumMessageBytes, false)
	if err != nil {
		return RedactedValue
	}
	return redacted
}

// SafeString returns a validated, redacted and UTF-8-safe string no longer
// than maximum bytes. It is intended for adapters that need a field-specific
// bound while keeping the same construction-time redaction policy.
func SafeString(value string, maximum int) (string, error) {
	return BoundedString(value, maximum, false)
}

// BoundedString redacts, strips control characters from and truncates value to
// maximum bytes. With required set it rejects a value that is empty afterwards.
func BoundedString(value string, maximum int, required bool) (string, error) {
	if maximum <= 0 || !utf8.ValidString(value) {
		return "", NewError(ErrorInvalidString, "construct telemetry")
	}
	value, _ = redact.Apply(value, safeStringRedactions)
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if required && value == "" {
		return "", NewError(ErrorInvalidString, "construct telemetry")
	}
	if len(value) > maximum {
		value = value[:maximum]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value, nil
}

// OpaqueID validates bounded identity fields without interpreting their
// spelling as a resource type. Free text and recognizable credential material
// must never become durable telemetry identity.
func OpaqueID(value string) bool {
	if len(value) < 3 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "pbce_") || strings.HasPrefix(lower, "pbnative_") {
		return false
	}
	for _, marker := range []string{"bearer", "password", "passwd", "secret", "token", "credential", "private_key", "api_key", "client_secret"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, rule := range secretStringRedactions {
		if rule.Pattern.MatchString(value) {
			return false
		}
	}
	return true
}

// StoredText reports whether value is already in the form BoundedString would
// produce, so a stored or imported value is never trusted without re-checking.
func StoredText(value string, maximum int) bool {
	clean, err := BoundedString(value, maximum, true)
	return err == nil && clean == value
}
