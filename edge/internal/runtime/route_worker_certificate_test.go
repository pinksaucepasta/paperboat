package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/edge/internal/tlscert"
)

type workerCertificateSource struct{ certificate *tls.Certificate }

func (s *workerCertificateSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.certificate == nil {
		return nil, tlscert.ErrCertificateMissing
	}
	return s.certificate, nil
}
func TestRouteWorkerWaitsForManagedCertificate(t *testing.T) {
	public, thumb := durableWorkerIdentity(t)
	assignment := durableWorkerAssignment(public, thumb, "route_tls", "assignment_tls", route.TunnelHTTPSWSS)
	assignment.MatchType = route.MatchManagedExact
	assignment.PublicHost, assignment.MatchHostname = "test.tunnels.example.test", "test.tunnels.example.test"
	source := &workerCertificateSource{}
	probes := 0
	worker := &RouteWorker{Certificates: source, Ready: func(context.Context, []route.RouteRule) error { probes++; return nil }}
	if err := worker.probeCanonicalGroup(context.Background(), []control.RouteAssignment{assignment}); err == nil {
		t.Fatal("managed route ready before TLS certificate activation")
	}
	if probes != 0 {
		t.Fatal("carrier probed before certificate ready")
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"*.tunnels.example.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	source.certificate = &tls.Certificate{Certificate: [][]byte{der}}
	if err := worker.probeCanonicalGroup(context.Background(), []control.RouteAssignment{assignment}); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatalf("carrier probes=%d", probes)
	}
	assignment.MatchHostname = "test.other.example"
	if err := worker.probeCanonicalGroup(context.Background(), []control.RouteAssignment{assignment}); err == nil {
		t.Fatal("wrong certificate authorized readiness")
	}
	assignment.MatchType = route.MatchExact
	source.certificate = nil
	if err := worker.probeCanonicalGroup(context.Background(), []control.RouteAssignment{assignment}); err != nil {
		t.Fatalf("custom on-demand route changed: %v", err)
	}
}

// The new address of a tunnel being moved rides along as a domain binding next to the primary managed name: the
// route is not ready on an edge that cannot serve TLS for it, while a customer's own domain binding is not gated.
func TestRouteWorkerWaitsForTheCertificateOfAMovedAddress(t *testing.T) {
	public, thumb := durableWorkerIdentity(t)
	assignment := durableWorkerAssignment(public, thumb, "route_move", "assignment_move", route.TunnelHTTPSWSS)
	assignment.MatchType = route.MatchManagedExact
	assignment.PublicHost, assignment.MatchHostname = "old.tunnels.example.test", "old.tunnels.example.test"
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"*.tunnels.example.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	source := &workerCertificateSource{certificate: &tls.Certificate{Certificate: [][]byte{der}}}
	worker := &RouteWorker{Certificates: source, Ready: func(context.Context, []route.RouteRule) error { return nil }}
	moved := func(host string) error {
		assignment.DomainBindings = []control.DomainBinding{{ID: "mvbinding", Hostname: host, MatchType: route.MatchExact, Generation: 2}}
		return worker.probeCanonicalGroup(context.Background(), []control.RouteAssignment{assignment})
	}
	if err := moved("new.tunnels.example.test"); err != nil {
		t.Fatalf("moved address covered by the wildcard: %v", err)
	}
	// A wildcard that does not cover the moved name (here: another parent domain) is a customer domain, not gated.
	if err := moved("app.customer.example"); err != nil {
		t.Fatalf("customer domain binding gated on the platform certificate: %v", err)
	}
	// Without the certificate the primary name already fails; a one-name coverage certificate for the primary only
	// must not authorize the moved name.
	cert = &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"old.tunnels.example.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err = x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	source.certificate = &tls.Certificate{Certificate: [][]byte{der}}
	if err := moved("new.tunnels.example.test"); err == nil {
		t.Fatal("a certificate that does not cover the moved address authorized readiness")
	}
}
