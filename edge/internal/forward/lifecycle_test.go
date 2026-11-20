package forward_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// flakySource is a server whose peer directory can go away.
type flakySource struct {
	*fakeSource
	down atomic.Bool
}

func (f *flakySource) Peers(ctx context.Context) (nodemesh.PeersResponse, error) {
	if f.down.Load() {
		return nodemesh.PeersResponse{}, errors.New("control plane unreachable")
	}
	return f.fakeSource.Peers(ctx)
}

// withFlakyPeers makes n read its peer directory through a source that can fail, and trust a stale one for stale.
func withFlakyPeers(n *node, stale time.Duration, extra func(*forward.Config)) *flakySource {
	f := &flakySource{fakeSource: n.src}
	n.cfg = func(c *forward.Config) {
		c.Peers = f
		c.MaxPeerStale = stale
		if extra != nil {
			extra(c)
		}
	}
	return f
}

func TestMeshPeerExpiryClosesLinks(t *testing.T) {
	tr := newTrio(t)
	a, b := tr.node(t, "a"), tr.node(t, "b") // a dials b
	flaky := withFlakyPeers(a, 400*time.Millisecond, nil)
	b.held.put(appName)
	cluster(a, b)
	eventually(t, "a links b and learns its name", func() bool { return a.mesh.Stats().Links == 1 && holdersOf(a, appName) == "b" })

	flaky.down.Store(true)
	if a.mesh.Stats().Links != 1 {
		t.Fatal("trust must outlive a short control-plane outage")
	}
	eventually(t, "link closed and names withdrawn after the outage outlasted MaxPeerStale", func() bool {
		return a.mesh.Stats().Links == 0 && holdersOf(a, appName) == ""
	})
	time.Sleep(600 * time.Millisecond)
	if a.mesh.Stats().Links != 0 {
		t.Fatal("a link came back while the peer directory was expired")
	}

	flaky.down.Store(false)
	eventually(t, "link and names restored by the next good refresh", func() bool { return a.mesh.Stats().Links == 1 && holdersOf(a, appName) == "b" })
}

func TestPeerCurrentFalseWhenExpired(t *testing.T) {
	tr := newTrio(t)
	a, b := tr.node(t, "a"), tr.node(t, "b") // b never starts: only a's directory matters
	var skew atomic.Int64
	flaky := withFlakyPeers(a, time.Second, func(c *forward.Config) {
		c.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	})
	a.src.setPeers(b.peer())
	a.start()
	eventually(t, "b current", func() bool { return a.mesh.PeerCurrent("b") })
	if a.mesh.PeerCurrent("nobody") {
		t.Fatal("a node outside the directory is not current")
	}

	flaky.down.Store(true)
	time.Sleep(300 * time.Millisecond) // several failed refreshes, still inside MaxPeerStale
	if !a.mesh.PeerCurrent("b") {
		t.Fatal("the last good directory must stay trusted inside MaxPeerStale")
	}
	skew.Store(int64(2 * time.Second))
	eventually(t, "b no longer current once the directory expired", func() bool { return !a.mesh.PeerCurrent("b") })

	flaky.down.Store(false)
	eventually(t, "b current again", func() bool { return a.mesh.PeerCurrent("b") })
}

// shortLivedCertificate is a node carrier certificate that stops being valid after life.
func shortLivedCertificate(t *testing.T, life time.Duration) (tls.Certificate, string, time.Time) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: "edge carrier"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(life), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nodemesh.SPKIPin(leaf), leaf.NotAfter // x509 times have 1 s resolution
}

func TestMeshLinkEndsAtCertExpiry(t *testing.T) {
	tr := newTrio(t)
	a, b := tr.node(t, "a"), tr.node(t, "b") // a dials b
	var notAfter time.Time
	b.link, b.pin, notAfter = shortLivedCertificate(t, 4*time.Second)
	cluster(a, b)
	eventually(t, "link up", func() bool { return a.mesh.Stats().Links == 1 && b.mesh.Stats().Links == 1 })
	if !time.Now().Before(notAfter) {
		t.Fatal("the certificate expired before the link came up; the test is too slow to prove anything")
	}
	eventually(t, "link ended", func() bool { return a.mesh.Stats().Links == 0 })
	if time.Now().Before(notAfter) {
		t.Fatalf("the link ended %v before its peer's certificate expired", time.Until(notAfter))
	}
}
