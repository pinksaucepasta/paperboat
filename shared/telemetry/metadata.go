package telemetry

import (
	"encoding/json"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
)

const (
	maximumEventLogSize  = 4096
	maximumMetadataBytes = 1000
	maximumMetadataItems = 64
	maximumMetadataDepth = 8
)

// safeMetadataString is the value path for typed metadata. A value that is
// exactly an already-validated opaque identifier is safe to retain. Free-form
// text still goes through the complete redaction pipeline, including the
// generic identifier rule. This keeps useful correlation/resource metadata
// while preventing identifiers embedded in prose from becoming labels.
func safeMetadataString(value, key string) (string, error) {
	if value != "" && strings.HasSuffix(strings.ToLower(key), "_id") && validOpaqueMetadataID(value) {
		return value, nil
	}
	return safe.BoundedString(value, maximumMetadataBytes, false)
}

func validOpaqueMetadataID(value string) bool {
	if !resourceIDPattern.MatchString(value) || strings.ContainsAny(value, "/?#@%\\\r\n\t ") {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"secret", "token", "password", "credential", "authorization", "cookie"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return strings.Contains(value, "_") || strings.Contains(value, "-")
}

// safeJSONNumber is kept separate so metadata can accept json.Number without
// allowing NaN, infinities, or arbitrary custom values through the interface.
func safeJSONNumber(value json.Number) (json.Number, error) {
	if value == "" {
		return "", safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
	}
	if _, err := value.Float64(); err != nil {
		return "", safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
	}
	return value, nil
}
