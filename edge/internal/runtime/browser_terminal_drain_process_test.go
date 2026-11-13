package runtime

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
)

// TestBrowserTerminalDrainAcrossUpdates runs pb update's browser terminal drain across real processes: this edge
// (its carrier listener with mTLS, runtime admissions, route registry, browser terminal hub and policy, wired as
// cmd/paperboat-edge wires them, against a control plane that serves the machine's admissions) and the machine
// (cli cmd/browserterminal-wasm TestBrowserTerminalDrainMachine: one PTY session, three worker generations and the
// console's browser terminal client). The machine asserts what the browser sees: no close and no pause through two
// consecutive handovers, then exactly one 4301 reattach at the drain cap with no output lost or repeated.
//
// CI builds the machine test binary and sets PAPERBOAT_DRAIN_MACHINE_TEST_BINARY (workflow terminal-drain.yml).
func TestBrowserTerminalDrainAcrossUpdates(t *testing.T) {
	binary := os.Getenv("PAPERBOAT_DRAIN_MACHINE_TEST_BINARY")
	if binary == "" {
		t.Skip("needs the machine test binary: cd cli && go test -c -o drain-machine.test ./cmd/browserterminal-wasm")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	const node, epoch, runtimeHost, edgeHost = "edge_drain", "edge_epoch_drain_1", "machine.runtime.example.test", "edge.example.test"
	dir := t.TempDir()
	trust, err := control.NewProcessCarrierServerTrust(node, epoch, "127.0.0.1", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	hub := edgehttp.NewBrowserTerminalHub()
	worker := &RuntimeCarrierWorker{Source: drainSource{path: filepath.Join(dir, "admissions.json")}, Expected: expected, Routes: routes, Endpoints: endpointRegistry(t, epoch), BrowserTerminalHub: hub, Interval: 50 * time.Millisecond, Timeout: time.Second}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Shutdown(context.Background()) }()

	// The carrier listener as cmd/paperboat-edge's carrier component builds it.
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{trust.Certificate}, ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error { _, err := expected.PeerBinding(state); return err }}
	carrierConfig := datacarrier.DefaultConfig()
	carrierConfig.MaximumStreams = 256
	carrierConfig.Authorize = expected
	endpoint := datacarrier.EndpointConfig{Address: "127.0.0.1:0", TLS: tlsConfig, PeerBinding: expected.PeerBinding}
	quic := endpoint
	service, err := datacarrier.NewHTTPService(ctx, datacarrier.ServiceConfig{Carrier: carrierConfig, MaximumCarriers: 16, TCP: &endpoint, QUIC: &quic})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	go func() {
		for {
			server, err := service.Accept(ctx)
			if err != nil {
				return
			}
			go func() { _ = worker.Handle(ctx, server) }()
		}
	}()

	transport, err := edgehttp.NewDataCarrierPreviewTransport(edgehttp.DataCarrierPreviewTransportConfig{Registry: routes, StreamOpenTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tickets := &drainTickets{path: filepath.Join(dir, "tickets.json")}
	policy, err := edgehttp.New(edgehttp.Config{
		PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 8192, MaxBodyBytes: 8192, Routes: edgehttp.NewPreviewCarrierRouteMatcher(routes),
		RuntimeCarrierTransport: transport, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: edgeHost, BrowserTerminal: tickets,
		OnFailure: func(_ context.Context, err error) { t.Logf("edge reported: %v", err) },
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	browsers := httptest.NewServer(policy)
	defer browsers.Close()

	describe, err := json.Marshal(map[string]string{"node_id": node, "epoch": epoch, "h2": "h2://" + service.TCPAddr().String(), "h3": "h3://" + service.QUICAddr().String(),
		"spki": trust.SPKISHA256, "chain": trust.CertificateChainPEM, "browser_url": "ws" + strings.TrimPrefix(browsers.URL, "http") + "/v1/browser-terminal/" + runtimeHost,
		"edge_host": edgeHost, "runtime_host": runtimeHost})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edge.json"), describe, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	machine := exec.CommandContext(ctx, binary, "-test.run=^TestBrowserTerminalDrainMachine$", "-test.count=1", "-test.v")
	machine.Env = append(os.Environ(), "PAPERBOAT_DRAIN_DIR="+dir)
	machine.Stdout, machine.Stderr = &output, &output
	if err := machine.Run(); err != nil {
		t.Fatalf("machine side failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "--- PASS: TestBrowserTerminalDrainMachine") {
		t.Fatalf("the machine side did not run its check:\n%s", output.String())
	}
	t.Log(lastLines(output.String(), 12))
}

// drainSource is the control plane's runtime carrier admissions for this edge: whatever the machine wrote last.
type drainSource struct{ path string }

func (s drainSource) RuntimeCarrierAdmissions(context.Context, string, string) ([]runtimewire.Admission, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var admissions []runtimewire.Admission
	return admissions, json.Unmarshal(raw, &admissions)
}
func (drainSource) EndpointAdmissions(context.Context, string, string, string) (control.EndpointUpdate, error) {
	return control.EndpointUpdate{Complete: true}, nil
}
func (drainSource) ObserveRuntimeCarriers(context.Context, string, string, []runtimewire.Observation) error {
	return nil
}

// drainTickets admits each browser connection with the next credential the machine minted for it.
type drainTickets struct {
	path string
	mu   sync.Mutex
	used int
}

func (d *drainTickets) Admit(context.Context, string, string, string) (control.BrowserTerminalAdmission, error) {
	raw, err := os.ReadFile(d.path)
	if err != nil {
		return control.BrowserTerminalAdmission{}, err
	}
	var tickets []struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AttachmentID      string `json:"attachment_id"`
	}
	if err := json.Unmarshal(raw, &tickets); err != nil {
		return control.BrowserTerminalAdmission{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.used >= len(tickets) {
		return control.BrowserTerminalAdmission{}, fmt.Errorf("browser connection %d has no ticket", d.used+1)
	}
	ticket := tickets[d.used]
	d.used++
	return control.BrowserTerminalAdmission{Credential: ticket.Credential, TerminalSessionID: ticket.TerminalSessionID, AttachmentID: ticket.AttachmentID,
		Closed: make(chan struct{}), Close: func() {}}, nil
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
