package runtimeattachment

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
)

// A healthy hub: a tunnels push refetches at once, otherwise the list is refetched only by the
// 5-minute safety poll (Tailscale pushes netmap deltas; the poll only bounds a missed push).
func TestEngineTunnelsRefetchOnPushAndFiveMinuteSafetyInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		changes := make(chan map[machineevents.Topic]int64)
		hub, err := machineevents.NewWithWaiter(func(ctx context.Context, _ map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
			select {
			case moved := <-changes:
				return moved, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		events := hub.Subscribe(machineevents.TopicTunnels)
		if err := hub.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer hub.Shutdown(context.Background())
		var fetches atomic.Int32
		service := newRuntimeAttachmentTestService(t, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			fetches.Add(1)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"tunnels":[]}}`)), Request: r}, nil
		}))
		service.tunnelsServed = true
		engine := &EngineTunnels{Service: service, Events: events}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); engine.run(ctx) }()
		synctest.Wait()
		if fetches.Load() != 1 {
			t.Fatalf("initial fetches = %d", fetches.Load())
		}
		time.Sleep(5*time.Minute - time.Second)
		synctest.Wait()
		if fetches.Load() != 1 {
			t.Fatalf("early safety fetch: %d", fetches.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if fetches.Load() != 2 {
			t.Fatalf("five minute fetches = %d", fetches.Load())
		}
		changes <- map[machineevents.Topic]int64{machineevents.TopicTunnels: 1}
		synctest.Wait()
		if fetches.Load() != 3 {
			t.Fatalf("push did not immediately refetch: %d", fetches.Load())
		}
		cancel()
		<-done
	})
}
