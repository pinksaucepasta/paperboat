//go:build !unix

package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// The terminal move fixture runs the unix host runtime, which imports this package on Windows
// (runtime -> hostinstall -> localdaemon -> tunnel); its tests are unix-only.
type connectedTerminalMove struct{ crashPhase string }

func newConnectedTerminalMove(t *testing.T, _ context.Context, _, _, _ string, _ ed25519.PublicKey, _ ed25519.PrivateKey) *connectedTerminalMove {
	t.Skip("the terminal move fixture needs the unix host runtime")
	return nil
}

func (f *connectedTerminalMove) prepareCarrier(context.Context, string, native.CarrierExchange) (native.CarrierOffer, error) {
	return native.CarrierOffer{}, errors.ErrUnsupported
}

func (f *connectedTerminalMove) serve(context.Context, *native.Session) error { return errors.ErrUnsupported }

func (f *connectedTerminalMove) exercise(t *testing.T, _ context.Context, _ *cliNativeRuntime, _ *native.Owner, _ *tailnet.Authority, _ config.ProfileStore, _ tls.Certificate, _ ed25519.PublicKey) {
	t.Skip("the terminal move fixture needs the unix host runtime")
}
