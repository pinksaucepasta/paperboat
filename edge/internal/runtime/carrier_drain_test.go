package runtime

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// An edge that stops (an install handover, or SIGTERM) first tells every carrier's host it is
// going away, then keeps serving: a held stream finishes before the drain ends, and the drain
// still ends at its deadline when a stream never does.
func TestCarrierAssemblyDrainSendsGoawayAndWaitsForOpenStreams(t *testing.T) {
	previous := drainMinimum
	drainMinimum = 200 * time.Millisecond
	t.Cleanup(func() { drainMinimum = previous })
	for _, mode := range []string{"stream finishes", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			identity := datacarrier.Identity{AccountID: "account_1", HostID: "host_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 1, Generation: 1}
			config := datacarrier.DefaultConfig()
			config.Identity = identity
			config.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
			edgeLink, hostLink := net.Pipe()
			server, err := datacarrier.NewServer(ctx, edgeLink, config)
			if err != nil {
				t.Fatal(err)
			}
			host, err := datacarrier.NewClient(ctx, hostLink, config)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			handled := make(chan struct{})
			var goaways atomic.Int32
			assembly, err := NewCarrierAssembly(CarrierAssemblyConfig{
				Service: newTelemetryCarrierService(server),
				Handle: func(ctx context.Context, _ *datacarrier.Server) error {
					close(handled)
					<-ctx.Done()
					return nil
				},
				Goaway: func(_ context.Context, got *datacarrier.Server, deadline time.Time) error {
					if got != server || deadline.IsZero() {
						t.Error("goaway for the wrong carrier or without a deadline")
					}
					goaways.Add(1)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := assembly.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = assembly.Shutdown(context.Background()) }()
			<-handled
			accepted := make(chan *datacarrier.Stream, 1)
			go func() {
				stream, _, err := host.AcceptStream(ctx)
				if err == nil {
					accepted <- stream
				}
			}()
			open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: identity.AccountID, TunnelID: identity.TunnelID, ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: 1, Generation: 1, RouteID: "route_1", RequestID: "request_held", Kind: "http"}
			held, err := server.OpenStream(ctx, open)
			if err != nil {
				t.Fatal(err)
			}
			hostSide := <-accepted
			drained := make(chan struct{})
			deadline := time.Now().Add(1500 * time.Millisecond)
			go func() { assembly.Drain(ctx, deadline); close(drained) }()
			select {
			case <-drained:
				t.Fatal("drain ended while a stream was still open")
			case <-time.After(500 * time.Millisecond):
			}
			if goaways.Load() != 1 {
				t.Fatalf("goaways = %d, want 1", goaways.Load())
			}
			if mode == "stream finishes" {
				_ = held.Close()
				_ = hostSide.Close()
				select {
				case <-drained:
				case <-time.After(time.Second):
					t.Fatal("drain did not end once the held stream finished")
				}
				if time.Now().After(deadline) {
					t.Fatal("drain waited for its deadline although the carrier was idle")
				}
				return
			}
			select {
			case <-drained:
				if time.Now().Before(deadline) {
					t.Fatal("drain ended before its deadline with a stream open")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("drain outlived its deadline")
			}
		})
	}
}
