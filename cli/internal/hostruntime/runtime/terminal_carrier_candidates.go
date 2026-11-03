//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

// The old authenticated native session supplies the root peer identity. The
// signed offer names an installer-custodied owner, not a caller-selected path.
func prepareTerminalCarrierPeer(root, catalog string) func(context.Context, string, native.CarrierExchange) (native.CarrierOffer, error) {
	return func(ctx context.Context, peer string, exchange native.CarrierExchange) (native.CarrierOffer, error) {
		instance := exchange.Offer.Claims.OwnerInstance
		if !sessionowner.ValidGeneration(instance) {
			return native.CarrierOffer{}, ErrProductionInvalid
		}
		descriptor, err := sessionowner.Read(filepath.Join(catalog, instance+".json"))
		if err != nil {
			return native.CarrierOffer{}, err
		}
		if descriptor.RuntimeRoot != root || descriptor.Source.SHA256[:32] != instance || validateIndependentAccount(descriptor) != nil {
			return native.CarrierOffer{}, ErrProductionInvalid
		}
		client, err := sessionowner.Connect(ctx, descriptor)
		if err != nil {
			return native.CarrierOffer{}, err
		}
		defer client.Close()
		body, err := json.Marshal(exchange)
		if err != nil {
			return native.CarrierOffer{}, err
		}
		response, err := client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "prepare-peer", PeerID: peer, Exchange: body})
		if err != nil {
			return native.CarrierOffer{}, err
		}
		return native.ParseCarrierOffer(response.Offer)
	}
}
