// Package tunnelendpoint owns the syntax shared by every server persistence
// writer for a managed tunnel endpoint identity.
package tunnelendpoint

import (
	"errors"
	"regexp"
)

var canonicalIDPattern = regexp.MustCompile(`^endpoint_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidateID accepts the canonical endpoint identity, independent of its hostname.
func ValidateID(value string) error {
	if !canonicalIDPattern.MatchString(value) {
		return errors.New("must be endpoint_ followed by a canonical lowercase UUIDv4")
	}
	return nil
}
