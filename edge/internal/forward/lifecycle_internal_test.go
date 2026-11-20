package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

type stubPeers struct{}

func (stubPeers) Peers(context.Context) (nodemesh.PeersResponse, error) {
	return nodemesh.PeersResponse{}, nil
}

// stubHolders answers every page with a next page, so a refresh only stops at its page cap; block (when set) holds
// each call until it is closed or the call's context ends.
type stubHolders struct {
	calls atomic.Int64
	block chan struct{}
}

func (s *stubHolders) Holders(ctx context.Context, _ string, _ int) (nodemesh.HoldersResponse, error) {
	s.calls.Add(1)
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nodemesh.HoldersResponse{}, ctx.Err()
		}
	}
	return nodemesh.HoldersResponse{Next: "next"}, nil
}

type stubHeld struct{}

func (stubHeld) Held() []string { return nil }

type stubInjector struct{}

func (stubInjector) Inject(net.Conn, net.Addr) error { return errors.New("not used") }

// internalMesh is a Mesh that is "started" (it has a context) but runs no loops, so a test drives it by hand. Its
// node id sorts after the test peers', so it never dials them on its own.
func internalMesh(t *testing.T, holders HoldersSource, dial func(context.Context, string, string) (net.Conn, error)) *Mesh {
	t.Helper()
	m, err := New(Config{NodeID: "z", Certificate: tls.Certificate{Certificate: [][]byte{{1}}}, Peers: stubPeers{}, Holders: holders,
		Held: stubHeld{}, Injector: stubInjector{}, DialTimeout: 5 * time.Second, OpenTimeout: 5 * time.Second, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	m.ctx, m.stop = context.WithCancel(context.Background())
	t.Cleanup(func() { m.stop(); m.closeLinks(); m.wg.Wait() })
	return m
}

func testPeer(id string, fill byte, epoch string) nodemesh.Peer {
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = fill
	}
	return nodemesh.Peer{NodeID: id, Region: "r1", Host: "192.0.2.1", Port: 9000, SPKISHA256: nodemesh.PinFromDigest(digest), ProcessEpoch: epoch}
}

func setPeers(t *testing.T, m *Mesh, peers ...nodemesh.Peer) {
	t.Helper()
	set, err := nodemesh.NewPeerSet(peers)
	if err != nil {
		t.Fatal(err)
	}
	m.applyPeers(m.ctx, set)
}

// testLink is a link for p on a pipe; far is the peer's end of the session.
func testLink(t *testing.T, m *Mesh, p nodemesh.Peer) (*link, *yamux.Session) {
	t.Helper()
	a, b := net.Pipe()
	near, err := yamux.Server(a, m.yamuxConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	far, err := yamux.Client(b, m.yamuxConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = near.Close(); _ = far.Close() })
	return &link{peer: p, sess: near, remote: a.RemoteAddr(), dialer: p.NodeID, started: time.Now()}, far
}

func TestMeshInstallRejectsDroppedPeer(t *testing.T) {
	m := internalMesh(t, &stubHolders{}, nil)
	b := testPeer("b", 1, "e1")
	cases := []struct {
		name string
		now  []nodemesh.Peer // the directory when install runs
		want bool
	}{
		{"still current", []nodemesh.Peer{b}, true},
		{"removed", nil, false},
		{"rekeyed", []nodemesh.Peer{testPeer("b", 2, "e1")}, false},
		{"new process epoch", []nodemesh.Peer{testPeer("b", 1, "e2")}, false},
		{"directory names no epoch", []nodemesh.Peer{testPeer("b", 1, "")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The handshake started when b was current; the directory moved before install ran.
			setPeers(t, m, b)
			l, _ := testLink(t, m, b)
			setPeers(t, m, c.now...)
			if _, installed := m.install(l); installed != c.want {
				t.Fatalf("install = %v, want %v", installed, c.want)
			}
			if !c.want && !l.sess.IsClosed() {
				t.Fatal("a refused link must be closed")
			}
			if !c.want && m.liveLink("b") != nil {
				t.Fatal("a refused link must not be recorded")
			}
			_ = l.sess.Close()
			setPeers(t, m) // next case starts empty
		})
	}
}

func TestMeshInFlightHandshakeCutWhenPeerRemoved(t *testing.T) {
	b := testPeer("b", 1, "e1")
	for _, c := range []struct {
		name string
		now  []nodemesh.Peer
	}{{"removed", nil}, {"rekeyed", []nodemesh.Peer{testPeer("b", 2, "e1")}}} {
		t.Run(c.name, func(t *testing.T) {
			reached := make(chan struct{})
			// The dial reaches the peer and then stalls, as a slow handshake would, until its context ends.
			m := internalMesh(t, &stubHolders{}, func(ctx context.Context, _, _ string) (net.Conn, error) {
				close(reached)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			setPeers(t, m, b)
			errc := make(chan error, 1)
			go func() { _, err := m.dial(m.ctx, b); errc <- err }()
			<-reached
			setPeers(t, m, c.now...)
			select {
			case err := <-errc:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("dial ended with %v, want a cancellation (not the 5 s dial timeout)", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the in-flight handshake of a removed peer was not cut")
			}
			if m.ctx.Err() != nil {
				t.Fatal("the mesh itself must keep running")
			}
		})
	}
}

// watchLink installs a link for b and returns the peer's end of its session.
func watchLink(t *testing.T, m *Mesh, b nodemesh.Peer) (*link, *yamux.Session) {
	t.Helper()
	l, far := testLink(t, m, b)
	if _, installed := m.install(l); !installed {
		t.Fatal("install refused a current peer")
	}
	return l, far
}

func TestWatchEndClosesLink(t *testing.T) {
	m := internalMesh(t, &stubHolders{}, nil)
	b := testPeer("b", 1, "")
	setPeers(t, m, b)
	l, far := watchLink(t, m, b)
	st, err := far.OpenStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStreamKind(st, streamWatch); err != nil {
		t.Fatal(err)
	}
	if err := writeWatch(st, nodemesh.WatchSnapshot, []string{"app.example.test"}, true); err != nil {
		t.Fatal(err)
	}
	if err := writeWatch(st, nodemesh.WatchEnd, nil, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "complete watch", func() bool { return m.dir.Complete("b") })
	_ = st.Close() // the watch ends; the session stays open on the peer's side
	select {
	case <-l.sess.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("a watch that ended left its link open, masking the server seed")
	}
	waitFor(t, "names withdrawn", func() bool { return !m.dir.Complete("b") && len(m.Holders("app.example.test")) == 0 })
}

func TestLinkDownKeepsReplacementWatch(t *testing.T) {
	m := internalMesh(t, &stubHolders{}, nil)
	b := testPeer("b", 1, "")
	setPeers(t, m, b)
	old, _ := watchLink(t, m, b)
	_ = old.sess.Close()
	fresh, _ := watchLink(t, m, b) // the replacement installs over the dead link
	waitFor(t, "old link teardown", func() bool { return m.liveLink("b") == fresh })
	snapshot := nodemesh.Watch{Kind: nodemesh.WatchSnapshot, Names: []string{"app.example.test"}}
	if err := m.dir.ApplyWatch("b", snapshot); err != nil {
		t.Fatal(err)
	}
	m.linkDown(old) // the old link's teardown, after the replacement's snapshot
	if !slices.Equal(m.Holders("app.example.test"), []string{"b"}) {
		t.Fatalf("the old link's teardown wiped the replacement's watch: %v", m.Holders("app.example.test"))
	}
	m.linkDown(fresh)
	if len(m.Holders("app.example.test")) != 0 {
		t.Fatal("the current link's teardown must withdraw the peer's names")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
