package native

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

func testOffer(name string) CarrierOffer {
	return CarrierOffer{Claims: CarrierClaims{Certificate: []byte(name)}}
}

// A carrier network that is slow to come up must not hold the lock every other peer's dial needs, and a second
// dial to the same generation waits only as long as its own context.
func TestSlowCarrierNetworkDoesNotBlockOtherNamespaces(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	slow := testOffer("slow")
	entered := make(chan struct{})
	errOther := errors.New("other namespace")
	o := &Owner{ctx: ctx, carrierNetworks: make(map[[32]byte]*carrierNetworkRef)}
	o.newCarrierNetwork = func(c context.Context, namespace [32]byte, _ func(mesh.ConnPacketConn)) (*tailnet.CarrierNetwork, error) {
		if namespace != slow.Namespace() {
			return nil, errOther
		}
		close(entered)
		<-c.Done()
		return nil, c.Err()
	}
	slowCtx, stopSlow := context.WithCancel(ctx)
	slowDone := make(chan error, 1)
	go func() { _, _, err := o.getCarrierNetwork(slowCtx, slow); slowDone <- err }()
	<-entered

	done := make(chan error, 1)
	go func() { _, _, err := o.getCarrierNetwork(ctx, testOffer("other")); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errOther) {
			t.Fatal("other namespace got", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a dial to another namespace waited on a slow carrier network")
	}

	waiterCtx, stopWaiter := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopWaiter()
	if _, _, err := o.getCarrierNetwork(waiterCtx, slow); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a second dial to the slow generation ended with", err)
	}
	stopSlow()
	if err := <-slowDone; !errors.Is(err, context.Canceled) {
		t.Fatal("the creating dial ended with", err)
	}
	if len(o.carrierNetworks) != 0 {
		t.Fatal("a failed creation left an entry behind")
	}
}
