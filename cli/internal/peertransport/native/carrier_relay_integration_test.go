package native_test

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

func startDelegatedRelayEcho(t *testing.T, ctx context.Context, owner *native.Owner, identity *native.CarrierIdentity) (*tailnet.CarrierNetwork, error) {
	t.Helper()
	apps, err := nativesession.New(nativesession.Config{
		Authorize: func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil },
		ServeStream: func(_ context.Context, _ streamauth.Header, connection net.Conn) error {
			_, err := io.Copy(connection, connection)
			return err
		},
		ServeTransfer: func(context.Context, net.Conn) error { return nil },
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(apps.Wait)
	return owner.StartCarrierNetwork(ctx, identity, "", apps.Serve)
}
