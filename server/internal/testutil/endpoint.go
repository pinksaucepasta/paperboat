// Package testutil contains deterministic values used by database-backed
// tests. Production endpoint allocation remains random and lives in
// tunnelv1.
package testutil

import (
	"crypto/sha256"
	"github.com/google/uuid"
)

// EndpointLabel returns a deterministic RFC-variant UUID suitable for a
// managed tunnel endpoint fixture. A seed keeps concurrent acceptance tests
// isolated without weakening the production allocation path.
func EndpointLabel(seed string) string {
	digest := sha256.Sum256([]byte("paperboat-test-endpoint:" + seed))
	digest[6] = (digest[6] & 0x0f) | 0x40
	digest[8] = (digest[8] & 0x3f) | 0x80
	value, _ := uuid.FromBytes(digest[:16])
	return value.String()
}

// EndpointID returns the current typed UUIDv4 fixture identity.
func EndpointID(seed string) string { return "endpoint_" + EndpointLabel(seed) }
