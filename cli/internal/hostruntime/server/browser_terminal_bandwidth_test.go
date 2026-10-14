package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
)

type browserUsageSink struct {
	mu      sync.Mutex
	reports []api.NativeUsageReport
}

func (s *browserUsageSink) ReportNativeUsage(_ context.Context, reports []api.NativeUsageReport, _, _ int64, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, reports...)
	return nil
}

func TestBrowserTerminalTLSApplicationBandwidthIsolation(t *testing.T) {
	sink := &browserUsageSink{}
	recorder, err := bandwidth.Open(filepath.Join(t.TempDir(), "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close(context.Background()) })
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	hostSocket, browserSocket := net.Pipe()
	hostTLS := tls.Server(hostSocket, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: private}}, NextProtos: []string{BrowserTerminalTLSALPN}})
	browserTLS := tls.Client(browserSocket, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost", NextProtos: []string{BrowserTerminalTLSALPN}})
	t.Cleanup(func() { _ = hostSocket.Close(); _ = browserSocket.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	handshakes := make(chan error, 1)
	go func() { handshakes <- hostTLS.HandshakeContext(ctx) }()
	if err := browserTLS.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-handshakes; err != nil {
		t.Fatal(err)
	}
	for _, attachment := range []string{"attachment-one", "attachment-two"} {
		// Each already-authorized attachment owns its binding, sharing one host recorder.
		plain := recorder.Wrap(hostTLS, bandwidth.Binding{AccessSessionID: attachment, StreamID: attachment, Consumer: "terminal"}, func(string) bandwidth.Path { return bandwidth.Path{Mode: "edge"} })
		application := newBrowserTerminalApplicationConnection(ctx, nil, plain)
		record := append([]byte{2, 0, 0, 0, 3}, []byte("abc")...)
		writes := make(chan error, 1)
		go func() { _, err := browserTLS.Write(record); writes <- err }()
		_, payload, err := application.ReadApplication()
		if err != nil || string(payload) != "abc" {
			t.Fatalf("application input %q, %v", payload, err)
		}
		if err := <-writes; err != nil {
			t.Fatal(err)
		}
		go func() { _, err := application.Write([]byte("output")); writes <- err }()
		var header [5]byte
		if _, err := io.ReadFull(browserTLS, header[:]); err != nil {
			t.Fatal(err)
		}
		output := make([]byte, binary.BigEndian.Uint32(header[1:]))
		if _, err := io.ReadFull(browserTLS, output); err != nil {
			t.Fatal(err)
		}
		if string(output) != "output" {
			t.Fatal("output changed")
		}
		if err := <-writes; err != nil {
			t.Fatal(err)
		}
		application.cancel()
	}
	if err := recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reports) != 2 {
		t.Fatalf("reports=%d, want isolated attachment counters", len(sink.reports))
	}
	seen := map[string]bool{}
	for _, report := range sink.reports {
		if report.Mode != "edge" || report.Consumer != "terminal" || report.NodeID != "" || report.UploadBytes != 8 || report.DownloadBytes != 11 || report.StreamID != report.AccessSessionID {
			t.Fatalf("incorrect application counter: %#v", report)
		}
		seen[report.AccessSessionID] = true
	}
	if !seen["attachment-one"] || !seen["attachment-two"] {
		t.Fatal("attachment bindings merged")
	}
}
