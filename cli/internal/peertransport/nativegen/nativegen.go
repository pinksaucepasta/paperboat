// Package nativegen reports a machine's active carrier generation to the control plane, which delivers its
// signed offer on the machine's entry of every peer's configuration, so clients that connect after the move
// (or were not connected during it) dial the generation directly (native dialOffered).
package nativegen

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

var control atomic.Pointer[api.Client]

// ErrNoControl means Use was not called in this process.
var ErrNoControl = errors.New("report carrier generation: no machine control client (nativegen.Use)")

// Use sets the machine-authenticated control client Report sends with; the carrier process calls it once
// with the client its authority refreshes over (api.Client.SetMachineAuth).
func Use(client *api.Client) { control.Store(client) }

// Report publishes offer as this machine's active carrier generation, replacing the last report; a zero offer
// (no signature) withdraws it. A generation re-reports each renewed offer: an offer lives at most 5 minutes.
func Report(ctx context.Context, offer native.CarrierOffer) error {
	client := control.Load()
	if client == nil {
		return ErrNoControl
	}
	var raw []byte
	if len(offer.Signature) != 0 {
		var err error
		if raw, err = offer.MarshalBinary(); err != nil {
			return err
		}
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	return client.ReportCarrierOffer(ctx, api.CarrierOfferReport{OperationID: "operation_" + id.String(), Offer: raw})
}
