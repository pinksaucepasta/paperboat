// Package nativegen publishes a machine's active carrier generation so clients
// that were not connected during an update can dial it.
package nativegen

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

// Report publishes offer as the machine's active carrier generation; the fresh-connect helper supplies the body.
func Report(ctx context.Context, offer native.CarrierOffer) error { return nil }
