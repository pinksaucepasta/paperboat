package main

import (
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

func TestAddressChangeMessageSaysWhereTheTunnelIs(t *testing.T) {
	mutation := func(endpoint, state string) api.TunnelMutation {
		return api.TunnelMutation{Tunnel: api.Tunnel{ID: "tun_1", StableEndpoint: endpoint}, Operation: api.TunnelOperation{State: state}}
	}
	if got := addressChangeMessage(mutation("https://old-name-1.pprbt.test", "running"), "hello"); !strings.Contains(got, "moving to https://hello.pprbt.test") || !strings.Contains(got, "keeps serving at https://old-name-1.pprbt.test") {
		t.Fatalf("pending change: %q", got)
	}
	if got := addressChangeMessage(mutation("https://old-name-1.pprbt.test", "succeeded"), "hello"); !strings.Contains(got, "now at https://hello.pprbt.test") {
		t.Fatalf("finished change: %q", got)
	}
	if got := addressChangeMessage(mutation("https://hello.pprbt.test", "running"), "hello"); !strings.Contains(got, "now at https://hello.pprbt.test") {
		t.Fatalf("change that finished while answering: %q", got)
	}
}
