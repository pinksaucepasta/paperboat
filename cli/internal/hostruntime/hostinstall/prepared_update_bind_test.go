package hostinstall

import (
	"context"
	"testing"
)

// The hooks of a supplied offline bundle are built from the request the helper resolved (its verified source),
// not from the request the caller sent, which has no source yet.
func TestPreparedUpdateHooksAreBoundToTheResolvedRequest(t *testing.T) {
	var seen string
	hooks := PreparedUpdateHooks{Bind: func(request Request) (func(context.Context) error, func(context.Context) error) {
		seen = request.Source.Version
		return func(context.Context) error { return nil }, func(context.Context) error { return nil }
	}}
	if hooks.Stage != nil || hooks.Validate != nil {
		t.Fatal("hooks built before the request is resolved")
	}
	resolved := Request{}
	resolved.Source.Version = "2026.11.06.4"
	bound := hooks.bound(resolved)
	if seen != "2026.11.06.4" || bound.Stage == nil || bound.Validate == nil {
		t.Fatalf("hooks not bound to the resolved request: seen=%q", seen)
	}
	if unbound := (PreparedUpdateHooks{}).bound(resolved); unbound.Stage != nil || unbound.Validate != nil {
		t.Fatal("hooks without Bind changed")
	}
}
