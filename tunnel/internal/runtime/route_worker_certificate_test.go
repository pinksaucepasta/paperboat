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

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/tlscert"
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
	assignment.PublicHost, assignment.MatchHostname = "test.tunnels.pprbt.dev", "test.tunnels.pprbt.dev"
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
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"*.tunnels.pprbt.dev"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
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
