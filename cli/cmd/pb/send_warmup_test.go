package main

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

// The warmup never fails or delays a send: without a peer, an identity or a
// generation there is nothing safe to pre-dial, so it returns synchronously
// and the send proceeds exactly as without it.
func TestPeerWarmupSkipsWithoutPeerOrIdentity(t *testing.T) {
	called := make(chan struct{}, 1)
	peer := daemonProbeFunc(func(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		called <- struct{}{}
		return localapi.PeerProbeResult{}, nil
	})
	warmPeerSessionBeforeDescriptor(context.Background(), nil, "machine_dst", "environment_dst", 7)
	warmPeerSessionBeforeDescriptor(context.Background(), peer, "", "environment_dst", 7)
	warmPeerSessionBeforeDescriptor(context.Background(), peer, "machine_dst", "", 7)
	warmPeerSessionBeforeDescriptor(context.Background(), peer, "machine_dst", "environment_dst", 0)
	select {
	case <-called:
		t.Fatal("warmup probed without a peer or an identity")
	case <-time.After(100 * time.Millisecond):
	}
}

// With an identity the warmup probes the peer on a health_probe stream in the
// background, with the short timeout: the caller never waits for it.
func TestPeerWarmupProbesTheDestinationPeer(t *testing.T) {
	probed := make(chan localapi.PeerStreamRequest, 1)
	peer := daemonProbeFunc(func(_ context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		probed <- request
		return localapi.PeerProbeResult{Path: "direct"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	warmPeerSessionBeforeDescriptor(ctx, peer, "machine_dst", "environment_dst", 7)
	if time.Since(started) > 2*time.Second {
		t.Fatal("warmup blocked the caller")
	}
	select {
	case request := <-probed:
		if err := request.Validate(time.Now()); err != nil {
			t.Fatalf("invalid warmup probe: %v", err)
		}
		if request.Consumer != "health_probe" || request.MachineID != "machine_dst" || request.EnvironmentID != "environment_dst" || request.MachineGeneration != 7 {
			t.Fatalf("wrong warmup probe: %+v", request)
		}
		if remaining := time.Until(request.Deadline); remaining <= 0 || remaining > peerWarmupTimeout {
			t.Fatalf("warmup deadline=%v, want within the short timeout", remaining)
		}
	case <-ctx.Done():
		t.Fatal("warmup never probed the destination peer")
	}
}

// A --to ref the daemon snapshot cannot resolve warms nothing: the machine
// identity comes from the prepare response, and pre-dialing without it is
// not safely possible, so the send proceeds cold as today.
