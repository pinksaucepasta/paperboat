package main

import (
	"context"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
)

// peerWarmupTimeout bounds the background session warm-up of a send: it never delays or fails the transfer.
const peerWarmupTimeout = 10 * time.Second

// warmPeerSessionBeforeDescriptor starts the native session dial to the destination before its transfer descriptor
// exists, so the cold engine start and first dial overlap hashing and the control-plane call instead of following
// them. Best effort: any error (offline destination, busy daemon) is dropped and the send proceeds exactly as without
// it. The health_probe consumer dials the peer path and opens no stream (as `pb ping` does); the transfer lease still
// does the full admission once the descriptor arrives.
func warmPeerSessionBeforeDescriptor(ctx context.Context, peer peerProber, machineID, environmentID string, generation int64) {
	if peer == nil || ctx == nil || strings.TrimSpace(machineID) == "" || strings.TrimSpace(environmentID) == "" || generation <= 0 {
		return
	}
	target := resolver.ConnectInfo{TargetKind: "machine", MachineID: machineID, MachineGeneration: uint64(generation), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: environmentID}}
	warmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerWarmupTimeout)
	go func() {
		defer cancel()
		_, _ = probeDaemonPeer(warmCtx, peer, target, newIdempotencyKey())
	}()
}
