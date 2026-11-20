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
	d := NewDirectory(10*time.Minute, func() time.Time { return now })
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
	d := NewDirectory(time.Minute, nil)
	d.SetRegion("home")
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
	d := NewDirectory(time.Minute, nil)
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
	d := NewDirectory(10*time.Minute, func() time.Time { return now })
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

func TestRefusedRemovesMatchingRevision(t *testing.T) {
	d := NewDirectory(time.Minute, nil)
	d.SetPeers([]nodemesh.Peer{peer("a", "r", false)})
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"x.example.test"}}); err != nil {
		t.Fatal(err)
	}
	holders := d.Holders("x.example.test")
	if len(holders) != 1 || holders[0].Rev == 0 {
		t.Fatalf("watch holder without a revision: %+v", holders)
	}
	d.Refused("x.example.test", "a", holders[0].Rev)
	if got := d.Holders("x.example.test"); len(got) != 0 {
		t.Fatalf("a refusal of the observed announcement left %v", got)
	}
}

func TestRefusedPreservesNewerWatchAdd(t *testing.T) {
	d := NewDirectory(time.Minute, nil)
	d.SetPeers([]nodemesh.Peer{peer("a", "r", false)})
	add := nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"x.example.test"}}
	if err := d.ApplyWatch("a", add); err != nil {
		t.Fatal(err)
	}
	observed := d.Holders("x.example.test")[0].Rev
	// The carrier dropped and came back while the Open was in flight: remove then add on the watch.
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchRemove, Names: add.Names}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyWatch("a", add); err != nil {
		t.Fatal(err)
	}
	d.Refused("x.example.test", "a", observed)
	if got := ids(d.Holders("x.example.test")); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a late refusal erased the newer announcement: %v", got)
	}
	// A repeated announcement without a removal is newer as well.
	observed = d.Holders("x.example.test")[0].Rev
	if err := d.ApplyWatch("a", add); err != nil {
		t.Fatal(err)
	}
	d.Refused("x.example.test", "a", observed)
	if len(d.Holders("x.example.test")) != 1 {
		t.Fatal("a refusal erased a re-announcement")
	}
	// The seed entry goes whatever the revision.
	d.ApplySeed([]nodemesh.Holder{{Name: "y.example.test", NodeIDs: []string{"a"}}})
	d.Refused("y.example.test", "a", 0)
	if len(d.Holders("y.example.test")) != 0 {
		t.Fatal("seed entry survived a refusal")
	}
}

func TestDirectoryWildcardFallbackExactFirst(t *testing.T) {
	d := NewDirectory(time.Minute, nil)
	d.SetPeers([]nodemesh.Peer{peer("a", "r", false), peer("b", "r", false)})
	wild := nodemesh.WildcardKey("apps.example.test")
	if err := d.ApplyWatch("a", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{wild}}); err != nil {
		t.Fatal(err)
	}
	if got := ids(d.Holders("one.apps.example.test")); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("concrete name did not match the wildcard: %v", got)
	}
	// One label only: the apex and a two-label name do not match.
	if len(d.Holders("apps.example.test")) != 0 || len(d.Holders("x.one.apps.example.test")) != 0 {
		t.Fatal("wildcard matched more than one label")
	}
	// An exact holder wins over the wildcard.
	if err := d.ApplyWatch("b", nodemesh.Watch{Kind: nodemesh.WatchAdd, Names: []string{"one.apps.example.test"}}); err != nil {
		t.Fatal(err)
	}
	if got := ids(d.Holders("one.apps.example.test")); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("exact holder must precede the wildcard: %v", got)
	}
	// A refusal of a concrete name does not unlist the wildcard for the other names.
	other := d.Holders("two.apps.example.test")[0]
	d.Refused("two.apps.example.test", "a", other.Rev)
	if got := ids(d.Holders("three.apps.example.test")); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("a refused concrete name hid the wildcard: %v", got)
	}
	// A raw TCP key never matches a wildcard.
	if len(d.Holders(nodemesh.TCPKey(30000))) != 0 {
		t.Fatal("tcp key matched")
	}
}
