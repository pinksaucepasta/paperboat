package main

import (
	"context"
	"errors"
	"net"
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
	notFound := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "dns-check.local.example.test", IsNotFound: true}
	}
	timeout := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: "dns-check.local.example.test", IsTimeout: true}
	}
	for name, lookup := range map[string]func(context.Context, string) ([]string, error){"empty answer": notFound, "wrong answer": filtered} {
		got := doctorLocalNamesCheck(t.Context(), "local.example.test", lookup)
		if got.Status != doctorpkg.StatusWarning || !strings.Contains(got.Summary, "DNS rebinding protection") ||
			!strings.Contains(got.Recovery, "Allow local.example.test in your router or Pi-hole") || !strings.Contains(got.Recovery, "127.100.x.y") {
			t.Fatalf("%s: %+v", name, got)
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("%s: invalid check: %v", name, err)
		}
	}
	for name, lookup := range map[string]func(context.Context, string) ([]string, error){"offline": offline, "timeout": timeout} {
		got := doctorLocalNamesCheck(t.Context(), "local.example.test", lookup)
		if got.Status != doctorpkg.StatusWarning || strings.Contains(got.Summary, "rebinding") || !strings.Contains(got.Recovery, "pb resolve") {
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
