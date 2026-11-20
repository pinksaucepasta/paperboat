package forward

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

func peer(id, region string, draining bool) nodemesh.Peer {
	return nodemesh.Peer{NodeID: id, Region: region, Host: "192.0.2.1", Port: 443, SPKISHA256: "sha256:" + fmt.Sprintf("%064x", len(id)), Draining: draining}
}

func ids(holders []Holder) []string {
	out := make([]string, len(holders))
	for i, h := range holders {
		out[i] = h.NodeID
	}
	return out
}

func TestSeedWinsUntilWatchComplete(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := NewDirectory("r1", 10*time.Minute, func() time.Time { return now })
	d.SetPeers([]nodemesh.Peer{peer("a", "r1", false), peer("b", "r1", false)})
	d.ApplySeed([]nodemesh.Holder{{Name: "x.example.test", NodeIDs: []string{"a", "b"}}})
	if got := ids(d.Holders("x.example.test")); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("seed before any watch: %v", got)
	}
	// a's watch is complete and does not list x: the watch wins over the seed for a; b still rides on the seed.
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchSnapshot, Names: []string{"y.example.test"}, More: true}); err != nil {
		t.Fatal(err)
	}
	if got := ids(d.Holders("x.example.test")); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("seed still counts while a's snapshot is partial: %v", got)
	}
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchEnd}); err != nil {
		t.Fatal(err)
	}
	if got := ids(d.Holders("x.example.test")); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("complete watch must win: %v", got)
	}
	if got := ids(d.Holders("y.example.test")); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("watch entry: %v", got)
	}
	// The link breaks: the watch is withdrawn and the seed covers the gap again.
	d.DropPeer("a")
	if got := ids(d.Holders("x.example.test")); !slices.Equal(got, []string{"a", "b"}) || len(d.Holders("y.example.test")) != 0 {
		t.Fatalf("after the link broke: %v", got)
	}
	// Seed entries expire unless refreshed.
	now = now.Add(11 * time.Minute)
	if len(d.Holders("x.example.test")) != 0 {
		t.Fatal("expired seed entry still listed")
	}
	d.Sweep()
	if d.Names() != 0 {
		t.Fatalf("names after the sweep: %d", d.Names())
	}
}

func TestDirectoryOrdersNonDrainingThenRegionThenID(t *testing.T) {
	d := NewDirectory("home", time.Minute, nil)
	d.SetPeers([]nodemesh.Peer{peer("a", "far", true), peer("b", "far", false), peer("cc", "home", false), peer("dd", "home", true), peer("e", "home", false)})
	for _, id := range []string{"a", "b", "cc", "dd", "e"} {
		if err := d.ApplyWatch(id, nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"x.example.test"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := ids(d.Holders("x.example.test")), []string{"cc", "e", "b", "dd", "a"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	// A peer that leaves the directory is forgotten along with what it held.
	d.SetPeers([]nodemesh.Peer{peer("b", "far", false)})
	if got := ids(d.Holders("x.example.test")); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("after peers changed: %v", got)
	}
	if err := d.ApplyWatch("cc", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"x.example.test"}}); err == nil {
		t.Fatal("a watch from a node outside the directory was applied")
	}
}

func TestDirectoryBoundsOnePeersNames(t *testing.T) {
	defer func(old int) { maxKeysPerPeer = old }(maxKeysPerPeer)
	maxKeysPerPeer = 1000
	d := NewDirectory("r", time.Minute, nil)
	d.SetPeers([]nodemesh.Peer{peer("a", "r", false)})
	names := make([]string, 0, maxKeysPerPeer)
	for i := range maxKeysPerPeer {
		names = append(names, fmt.Sprintf("n%d.example.test", i))
	}
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: names}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"one-more.example.test"}}); err == nil {
		t.Fatal("a peer claimed more names than a node can hold")
	}
	// Names already held are not new claims.
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: names[:10]}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryTCPPortsListsWatchedAndSeededPortsOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := NewDirectory("r", 10*time.Minute, func() time.Time { return now })
	d.SetPeers([]nodemesh.Peer{peer("a", "r", false), peer("b", "r", false)})
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchSnapshot, Names: []string{nodemesh.TCPKey(30000), "x.example.test"}}); err != nil {
		t.Fatal(err)
	}
	d.ApplySeed([]nodemesh.Holder{{Name: nodemesh.TCPKey(20000), NodeIDs: []string{"b"}}, {Name: nodemesh.TCPKey(40000), NodeIDs: []string{"gone"}}})
	if got := d.TCPPorts(); !slices.Equal(got, []uint16{20000, 30000}) {
		t.Fatalf("ports %v", got)
	}
	now = now.Add(11 * time.Minute) // the seed expires; a's watch entry stays
	if got := d.TCPPorts(); !slices.Equal(got, []uint16{30000}) {
		t.Fatalf("ports after the seed expired %v", got)
	}
}
