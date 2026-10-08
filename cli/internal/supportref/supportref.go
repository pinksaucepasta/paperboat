// Package supportref owns the opaque reference used to correlate one pb
// invocation with control-plane and local diagnostic evidence.
package supportref

import (
	"context"
	"github.com/google/uuid"
	"regexp"
)

const Header = "Support-Reference"

var valid = regexp.MustCompile(`^support_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type contextKey struct{}

func New() string {
	id, err := uuid.NewRandom()
	if err != nil {
		return ""
	}
	return "support_" + id.String()
}

func Valid(value string) bool {
	return valid.MatchString(value)
}

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
