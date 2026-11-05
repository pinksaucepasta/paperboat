package machineguard

import (
	"errors"
	"net/netip"
	"testing"
)

// A client that sent its request and half-closed before the proxy looked is no longer ESTABLISHED but still names
// its user: the proxy attributes it, the gateway (which hands over a descriptor) still refuses it, and a
// TIME_WAIT socket (uid 0) is refused by both.
func TestLiveAttributionAcceptsAHalfClosedClientOnly(t *testing.T) {
	tuple := socketTuple{Source: netip.MustParseAddrPort("127.0.0.1:50000"), Destination: netip.MustParseAddrPort("127.0.0.1:60000")}
	answer := func(o socketOwner) lookupOwners {
		return func(socketTuple) ([]socketOwner, error) { o.Tuple = tuple; return []socketOwner{o}, nil }
	}
	halfClosed := answer(socketOwner{UID: 501, Live: true})
	if uid, err := attributeLiveOwner(tuple, halfClosed); err != nil || uid != 501 {
		t.Fatalf("half-closed client: uid %d, %v; want 501", uid, err)
	}
	if _, err := attributeOwner(tuple, halfClosed); !errors.Is(err, errAttributionNotFound) {
		t.Fatalf("the gateway must still refuse a not-established client: %v", err)
	}
	timeWait := answer(socketOwner{UID: 0})
	for name, attribute := range map[string]func(socketTuple, lookupOwners) (uint32, error){"live": attributeLiveOwner, "gateway": attributeOwner} {
		if _, err := attribute(tuple, timeWait); !errors.Is(err, errAttributionNotFound) {
			t.Fatalf("%s: a TIME_WAIT socket (uid 0) must be refused: %v", name, err)
		}
	}
}
