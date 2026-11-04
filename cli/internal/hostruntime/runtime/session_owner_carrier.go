//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	nativeservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

// relayReason says whether the candidate exchange named a relay (bounded).
func relayReason(nodeID string) string {
	if nodeID == "" {
		return "no_relay"
	}
	return "relay"
}

func (o *IndependentSessionOwner) controlCarrier(ctx context.Context, request workloadbridge.CarrierRequest) (workloadbridge.CarrierResponse, error) {
	o.carrierMu.Lock()
	defer o.carrierMu.Unlock()
	var result workloadbridge.CarrierResponse
	switch request.Operation {
	case "prepare-peer":
		if request.Slot != 0 || len(request.Offer) != 0 || len(request.Exchange) > 12<<10 || request.PeerID == "" || o.carrier == nil || !o.carrier.ready() {
			return result, ErrProductionInvalid
		}
		var exchange native.CarrierExchange
		if json.Unmarshal(request.Exchange, &exchange) != nil || !bytes.Equal(exchange.Offer.Claims.Certificate, o.carrier.offer.Claims.Certificate) {
			return result, ErrProductionInvalid
		}
		if err := readyCarrierPeer(ctx, o.carrier.network, request.PeerID, exchange); err != nil {
			return result, err
		}

	case "seal":
		if request.Slot != 0 || len(request.Offer) != 0 || o.carrier == nil {
			return result, ErrProductionInvalid
		}
		o.carrier.network.Seal()
	case "status":
		if request.Slot != 0 || len(request.Offer) != 0 {
			return result, ErrProductionInvalid
		}
	case "start":
		if request.Slot < 1 || request.Slot > 2 || len(request.Offer) != 0 || o.carrier != nil {
			return result, ErrProductionInvalid
		}
		environ, err := o.carrierEnvironment(ctx)
		if err != nil {
			return result, err
		}
		staging, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(o.lifetime, cancel)
		defer stop()
		o.carrier, err = newTerminalCarrier(staging, buildinfo.Version, environ, request.Slot, o.durable)
		if err != nil {
			return result, err
		}
	case "move":
		if request.Slot != 0 || len(request.Offer) == 0 {
			return result, ErrProductionInvalid
		}
		offer, err := native.ParseCarrierOffer(request.Offer)
		if err != nil {
			return result, err
		}
		if o.carrier != nil {
			if err := o.carrier.owner.OfferCarrier(ctx, offer); err != nil {
				return result, err
			}
		}
	case "stop":
		if request.Slot != 0 || len(request.Offer) != 0 {
			return result, ErrProductionInvalid
		}
		return result, o.stopCarrierLocked(ctx)
	default:
		return result, ErrProductionInvalid
	}
	if o.carrier != nil {
		result.Slot = o.carrier.slot
		if o.carrier.ready() {
			var err error
			result.Offer, err = o.carrier.offer.MarshalBinary()
			return result, err
		}
	}
	return result, nil
}

func (o *IndependentSessionOwner) stopCarrier(ctx context.Context) error {
	o.carrierMu.Lock()
	defer o.carrierMu.Unlock()
	return o.stopCarrierLocked(ctx)
}

func (o *IndependentSessionOwner) stopCarrierLocked(ctx context.Context) error {
	if o.carrier == nil {
		return nil
	}
	err := o.carrier.Close(ctx)
	o.carrier = nil
	return err
}

func (o *IndependentSessionOwner) carrierEnvironment(ctx context.Context) (func(string) string, error) {
	cfg, err := hostinstall.ReadCarrierDescriptor(ctx, o.descriptor.Instance)
	if err != nil {
		return nil, err
	}
	if cfg.UID != o.descriptor.UID || cfg.GID != o.descriptor.GID || cfg.RuntimeRoot != o.descriptor.RuntimeRoot {
		return nil, ErrProductionInvalid
	}
	layout, err := nativeservice.UserLayout(o.descriptor.Source.Platform, o.descriptor.UID)
	if o.descriptor.OwnerSID != "" {
		layout, err = nativeservice.WindowsUserLayout(o.descriptor.OwnerSID)
	}
	if err != nil {
		return nil, err
	}
	return func(key string) string {
		switch key {
		case "PAPERBOAT_UPDATED_SOCKET":
			return layout.UpdaterSocket
		case "PAPERBOAT_SESSION_OWNER_CATALOG":
			return filepath.Clean(o.catalogRoot)
		case "PAPERBOAT_WORKSPACE_ROOT":
			return o.descriptor.Workspace
		case "PAPERBOAT_SHELL":
			return o.descriptor.Shell
		default:
			return cfg.Environment[key]
		}
	}, nil
}
