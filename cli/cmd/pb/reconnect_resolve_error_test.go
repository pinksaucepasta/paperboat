package main

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

// X-21: after a team grant was revoked the member's `pb connect` kept reconnecting for minutes, because only
// machine_revoked ended it while Paperboat answers a lost grant, unshare or removal with 404/403.
func TestReconnectResolveErrorEndsTheTerminalOnARefusedGrant(t *testing.T) {
	revoked := reflect.TypeOf(errors.Unwrap(tunnel.Revoked(errors.New("x"))))
	for _, refusal := range []*api.APIError{
		{Status: http.StatusNotFound, Code: "user_machine_not_found"},
		{Status: http.StatusForbidden, Code: "machine_revoked"},
		{Status: http.StatusForbidden, Code: "workspace_unavailable"},
	} {
		got := reconnectResolveError(refusal)
		if inner := errors.Unwrap(got); inner == nil || reflect.TypeOf(inner) != revoked || !errors.Is(got, refusal) {
			t.Errorf("%s: %T %v is not a revoked refusal", refusal.Code, got, got)
		}
	}
	transient := &api.APIError{Status: http.StatusServiceUnavailable, Code: "connector_unavailable"}
	if got := reconnectResolveError(transient); got != error(transient) {
		t.Errorf("a transient refusal stopped the reconnect: %T %v", got, got)
	}
}
