//go:build darwin || linux || windows

package runtime

import (
	"context"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// readyCarrierPeer adds the client's candidates to a generation's network and
// punches toward it, so the client's dial finds a path.
func readyCarrierPeer(ctx context.Context, network *tailnet.CarrierNetwork, peerID string, exchange native.CarrierExchange) error {
	if err := network.AddPeer(peerID, exchange.DiscoPublicKey, exchange.Endpoints); err != nil {
		diagnosticlog.TryInfo("carrier shutdown", "stage", "prepare_peer_add", "failed", true, "error_type", fmt.Sprintf("%T", err))
		return err
	}
	// The relay is the fallback path; a refused relay registration must not
	// block a direct (punched) move. Dial below decides (pbvm4 run3b).
	if err := network.PreparePeerRelay(ctx, peerID, exchange.RelayNodeID); err != nil {
		diagnosticlog.TryInfo("carrier shutdown", "stage", "prepare_peer_relay", "failed", true, "error_type", fmt.Sprintf("%T", err), "reason", relayReason(exchange.RelayNodeID))
	}
	socket, err := network.Dial(ctx, peerID)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "prepare_peer_dial", "failed", err != nil, "error_type", fmt.Sprintf("%T", err), "reason", relayReason(exchange.RelayNodeID))
	if err != nil {
		return err
	}
	socket.Close()
	return nil
}

