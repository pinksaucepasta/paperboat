// Package operationrevocation is the host revocation feed contract: a host sends the
// IDs of its live operation credentials and the server answers which are revoked.
package operationrevocation

import (
	"errors"
	"strings"
)

// MaxLiveJTIs bounds one request's watch list and one response; hosts send larger sets in chunks.
const MaxLiveJTIs = 1024

var ErrLiveJTIsInvalid = errors.New("active operation credential IDs are invalid")

// ValidJTI reports whether a credential ID can be carried in the feed.
func ValidJTI(id string) bool {
	return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\x00\r\n")
}

// ValidateLive checks a request's active_jtis: present, bounded, valid and unique.
func ValidateLive(live []string) error {
	if live == nil || len(live) > MaxLiveJTIs {
		return ErrLiveJTIsInvalid
	}
	seen := make(map[string]bool, len(live))
	for _, id := range live {
		if !ValidJTI(id) || seen[id] {
			return ErrLiveJTIsInvalid
		}
		seen[id] = true
	}
	return nil
}
