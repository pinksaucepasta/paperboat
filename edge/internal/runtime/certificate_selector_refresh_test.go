package runtime

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"
)

type fixedSource struct{ cert *tls.Certificate }

func (f fixedSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return f.cert, nil
}

type countingRequester struct {
	calls atomic.Int32
	asked chan string
}

func (r *countingRequester) RequestCertificate(_ context.Context, host string) error {
	r.calls.Add(1)
	r.asked <- host
	return nil
}

// An edge that missed the push of a replacement (the message lives 10 minutes) asks for it while the old leaf
// is near expiry, keeps serving the old leaf meanwhile, and asks at most once a minute per name.
func TestCertificateSelectorRefreshesNearExpiryLeafInBackgroundOncePerInterval(t *testing.T) {
	cert, _ := wildcardTestCertificate(t, "app.example.test") // valid for one hour: inside the refresh window
	requester := &countingRequester{asked: make(chan string, 4)}
	selector, err := NewCertificateSelector(CertificateSelectorConfig{Source: fixedSource{cert}, OnDemandRequester: requester, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := selector.lookup(context.Background(), "app.example.test")
		if err != nil || got != cert {
			t.Fatalf("lookup = %v, %v; want the leaf it has", got, err)
		}
	}
	select {
	case host := <-requester.asked:
		if host != "app.example.test" {
			t.Fatalf("refresh asked for %q", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("near-expiry leaf was not refreshed")
	}
	time.Sleep(50 * time.Millisecond)
	if n := requester.calls.Load(); n != 1 {
		t.Fatalf("refresh requests = %d, want 1 within the interval", n)
	}
}

// A shared wildcard has no name of its own: it is never requested per host, and a leaf with time left is not either.
func TestCertificateSelectorDoesNotRefreshWildcardOrFreshLeaf(t *testing.T) {
	wild, _ := wildcardTestCertificate(t, "*.example.test")
	requester := &countingRequester{asked: make(chan string, 4)}
	selector, err := NewCertificateSelector(CertificateSelectorConfig{Source: fixedSource{wild}, OnDemandRequester: requester, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selector.lookup(context.Background(), "app.example.test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := requester.calls.Load(); n != 0 {
		t.Fatalf("wildcard served for a host caused %d refresh requests, want 0", n)
	}
}
