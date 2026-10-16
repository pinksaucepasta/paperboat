package preview

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
)

func TestMachineCarrierReplacementDrainsAdmittedStreamAndStillRevokes(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "revocation"}[revoke], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root, store := newMachineAttachmentIdentity(t)
			now := time.Now().UTC()
			id := testPreviewCarrierIdentity(1)
			id.TunnelID, id.ConnectorID = id.HostID, id.HostID
			admission := machineAttachmentAdmission(t, store, now, id, []string{"h2://edge.example.test:443"})
			var edges []*connector.DataCarrier
			factory := func(id connector.DataCarrierIdentity, config connector.DataCarrierPoolConfig, _ connector.NetworkDialerConfig) (connector.DataCarrierSessionSource, error) {
				local, remote := net.Pipe()
				edge, err := connector.NewDataCarrierServer(ctx, remote, config.Carrier, connector.DataCarrierAdmission{
					Identity: id, Authorize: func(context.Context, connector.StreamOpen) error { return nil },
				})
				if err != nil {
					local.Close()
					remote.Close()
					return connector.DataCarrierSessionSource{}, err
				}
				edges = append(edges, edge)
				return connector.NewDataCarrierSessionSource(id, config, func(_ context.Context, r connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
					return connector.DataCarrierDialResult{Link: local, PeerIdentity: id, Transport: r.Transport, EdgeID: r.EdgeID, FailureDomain: r.FailureDomain}, nil
				})
			}
			source, err := NewMachineAttachmentSessionSource(MachineAttachmentSessionSourceConfig{StateRoot: root, SessionFactory: factory})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				source.Close(context.Background())
				for _, edge := range edges {
					edge.Close()
				}
			})
			old, err := source.AcquirePreviewDataCarrier(ctx, admission)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := old.Active.OpenStream(ctx, testPreviewStreamOpen(id, "route_held", "request_held"))
			if err != nil {
				t.Fatal(err)
			}
			peer, _, err := edges[0].AcceptStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			defer peer.Close()
			admission.Binding.EdgeProcessEpoch = "edge_epoch_02"
			replacement, err := source.AcquirePreviewDataCarrier(ctx, admission)
			if err != nil || replacement.Active == old.Active {
				t.Fatalf("replacement=%v error=%v", replacement.Active, err)
			}
			if err = old.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = old.Active.OpenStream(ctx, testPreviewStreamOpen(id, "route_held", "request_late")); !errors.Is(err, connector.ErrDataCarrierDraining) {
				t.Fatalf("retired carrier admitted a new stream: %v", err)
			}
			assertRetiredCarrierExchange(t, ctx, stream, peer)
			if revoke {
				if err = source.Close(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err = stream.Write([]byte("revoked")); err == nil {
					t.Fatal("revocation retained a retired stream")
				}
			} else {
				stream.Close()
				peer.Close()
				for old.Active.Pool().State() != connector.DataCarrierPoolClosed {
					select {
					case <-ctx.Done():
						t.Fatal("completed retired carrier was not reclaimed")
					case <-time.After(time.Millisecond):
					}
				}
				if err = replacement.Release(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func assertRetiredCarrierExchange(t *testing.T, ctx context.Context, stream, peer io.ReadWriteCloser) {
	t.Helper()
	for _, direction := range [][2]io.ReadWriteCloser{{stream, peer}, {peer, stream}} {
		read := make(chan error, 1)
		go func() {
			body := make([]byte, 9)
			_, err := io.ReadFull(direction[1], body)
			if err == nil && string(body) != "admitted!" {
				err = errors.New("retired stream changed admitted bytes")
			}
			read <- err
		}()
		if _, err := direction[0].Write([]byte("admitted!")); err != nil {
			t.Fatalf("replacement closed admitted stream: %v", err)
		}
		select {
		case err := <-read:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("replacement stalled admitted stream")
		}
	}
}
