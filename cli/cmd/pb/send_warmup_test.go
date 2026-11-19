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
	for name, skipped := range map[string]func() bool{
		"no machine":     func() bool { _, ok := peerWarmupTarget("", "environment_dst", 7); return !ok },
		"blank machine":  func() bool { _, ok := peerWarmupTarget("  ", "environment_dst", 7); return !ok },
		"no environment": func() bool { _, ok := peerWarmupTarget("machine_dst", "", 7); return !ok },
		"no generation":  func() bool { _, ok := peerWarmupTarget("machine_dst", "environment_dst", 0); return !ok },
	} {
		if !skipped() {
			t.Fatalf("%s: warmup would probe without a complete identity", name)
		}
	}
	if _, ok := peerWarmupTarget("machine_dst", "environment_dst", 7); !ok {
		t.Fatal("a complete identity must warm")
	}
	// Without a peer nothing is dialed and nothing is started: the call returns with no goroutine to wait for.
	warmPeerSessionBeforeDescriptor(context.Background(), nil, "machine_dst", "environment_dst", 7)
}

// With an identity the warmup probes the peer on a health_probe stream in the
// background, with the short timeout: the caller never waits for it.
func TestPeerWarmupProbesTheDestinationPeer(t *testing.T) {
	probed := make(chan localapi.PeerStreamRequest, 1)
	release := make(chan struct{})
	defer close(release)
	peer := daemonProbeFunc(func(_ context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		probed <- request
		<-release // a probe that never finishes must not hold the caller
		return localapi.PeerProbeResult{Path: "direct"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	returned := make(chan struct{})
	go func() {
		warmPeerSessionBeforeDescriptor(ctx, peer, "machine_dst", "environment_dst", 7)
		close(returned)
	}()
	select {
	case <-returned:
	case <-ctx.Done():
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
