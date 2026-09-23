// Package supportref owns the opaque reference used to correlate one pb
// invocation with control-plane and local diagnostic evidence.
package supportref

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

const Header = "Support-Reference"

var valid = regexp.MustCompile(`^pb-[0-9a-f]{32}$`)

type contextKey struct{}

func New() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	return "pb-" + hex.EncodeToString(value[:])
}

func Valid(value string) bool { return valid.MatchString(value) }

func WithContext(ctx context.Context, value string) context.Context {
	if !Valid(value) {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, value)
}

func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(contextKey{}).(string)
	if Valid(value) {
		return value
	}
	return ""
}
