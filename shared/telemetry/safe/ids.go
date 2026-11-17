package safe

import "regexp"

var safeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,127}$`)

// HasPrefix reports whether value is a safe opaque ID that starts with one of
// prefixes and carries something after it.
func HasPrefix(value string, prefixes ...string) bool {
	if !safeIDPattern.MatchString(value) || !OpaqueID(value) {
		return false
	}
	for _, prefix := range prefixes {
		if len(value) > len(prefix) && value[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// CorrelationID reports whether value is a correlation or request identity a
// producer may attach to health, events and logs. Producers mint prefixed IDs
// (corr_, req_, support_) and UUIDs, so the rule is the opaque-ID rule.
func CorrelationID(value string) bool { return OpaqueID(value) }

var stableCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// StableCode reports whether value is a machine-readable code: lowercase,
// digits and underscores, starting with a letter.
func StableCode(value string) bool { return stableCodePattern.MatchString(value) }
