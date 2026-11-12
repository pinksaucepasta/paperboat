package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// F21-50: local browser names are a public wildcard answering the loopback gateway; with no DNS or behind a
// DNS-rebinding filter they do not resolve, and doctor says what to do instead of leaving a browser error.
func TestDoctorLocalNamesSaysWhatToDoWhenTheNamesDoNotResolve(t *testing.T) {
	resolves := func(context.Context, string) ([]string, error) { return []string{splitdns.BrowserGatewayIP}, nil }
	offline := func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	filtered := func(context.Context, string) ([]string, error) { return []string{"203.0.113.9"}, nil }

	if got := doctorLocalNamesCheck(t.Context(), "local.example.test", resolves); got.Status != doctorpkg.StatusPass {
		t.Fatalf("resolving names: %+v", got)
	}
	for name, lookup := range map[string]func(context.Context, string) ([]string, error){"offline": offline, "wrong answer": filtered} {
		got := doctorLocalNamesCheck(t.Context(), "local.example.test", lookup)
		if got.Status != doctorpkg.StatusWarning || !strings.Contains(got.Recovery, "hosts file") || !strings.Contains(got.Recovery, splitdns.BrowserGatewayIP+" <name>.local.example.test") {
			t.Fatalf("%s: %+v", name, got)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("%s: invalid check: %v", name, err)
		}
	}
	if got := doctorLocalNamesCheck(t.Context(), "", resolves); got.Status != doctorpkg.StatusUnavailable {
		t.Fatalf("not set up: %+v", got)
	}
}
