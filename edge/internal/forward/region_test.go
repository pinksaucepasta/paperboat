package forward

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// regionPeers is the server's peer answer: the directory and the caller's region, both set by the test.
type regionPeers struct {
	region string
	peers  []nodemesh.Peer
}

func (s *regionPeers) Peers(context.Context) (nodemesh.PeersResponse, error) {
	return nodemesh.PeersResponse{Region: s.region, Peers: s.peers}, nil
}

type noHolders struct{}

func (noHolders) Holders(context.Context, string, int) (nodemesh.HoldersResponse, error) {
	return nodemesh.HoldersResponse{}, nil
}

type noHeld struct{}

func (noHeld) Held() []string { return nil }

type noInjector struct{}

func (noInjector) Inject(net.Conn, net.Addr) error { return nil }

// The region comes from the peers answer: until one names it, holders order by id alone; an answer without a
// region keeps the last one.
func TestDirectoryPrefersSameRegionFromPeersAnswer(t *testing.T) {
	src := &regionPeers{peers: []nodemesh.Peer{peer("a-far", "far", false), peer("b-home", "home", false)}}
	m, err := New(Config{NodeID: "me", Certificate: tls.Certificate{Certificate: [][]byte{{0}}}, Peers: src, Holders: noHolders{}, Held: noHeld{}, Injector: noInjector{}})
	if err != nil {
		t.Fatal(err)
	}
	d := m.Directory()
	ctx := context.Background()
	m.refreshPeers(ctx)
	for _, id := range []string{"a-far", "b-home"} {
		if err := d.ApplyWatch(id, nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"x.example.test"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := ids(d.Holders("x.example.test")), []string{"a-far", "b-home"}; !slices.Equal(got, want) {
		t.Fatalf("before the answer names a region: %v, want %v", got, want)
	}
	src.region = "home"
	m.refreshPeers(ctx)
	if got, want := ids(d.Holders("x.example.test")), []string{"b-home", "a-far"}; !slices.Equal(got, want) {
		t.Fatalf("after the answer names the region: %v, want %v", got, want)
	}
	src.region = ""
	m.refreshPeers(ctx)
	if got, want := ids(d.Holders("x.example.test")), []string{"b-home", "a-far"}; !slices.Equal(got, want) {
		t.Fatalf("an answer without a region dropped the last one: %v, want %v", got, want)
	}
}
