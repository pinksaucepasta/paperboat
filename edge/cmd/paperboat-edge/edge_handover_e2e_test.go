package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat/edge/internal/handover"
	edgeruntime "github.com/pinksaucepasta/paperboat/edge/internal/runtime"
	"github.com/pinksaucepasta/paperboat/edge/internal/tlscert"
)

const handoverCertificateHost = "app.example.test"

// handoverRegistry is a process's TLS certificate registry. The serving process holds one active
// certificate that the server delivered once and acknowledged; the server never sends it again for
// this epoch, so the replacement has it only if the serving process hands it over.
func handoverRegistry(t *testing.T, role string) *tlscert.Registry {
	t.Helper()
	registry, err := tlscert.NewRegistry(tlscert.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if role != "old" {
		return registry
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: handoverCertificateHost}, DNSNames: []string{handoverCertificateHost}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	binding := tlscert.Binding{AccountID: "account_h", TunnelID: "tunnel_h", DomainID: "domain_h", Hostname: handoverCertificateHost, DomainGeneration: 1, CertificateGeneration: 1, EdgeNodeID: handoverNode, EdgeProcessEpoch: handoverEpoch, AssignmentGeneration: 1}
	bundle := tlscert.Bundle{CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), Issuer: "test-ca", NotBefore: template.NotBefore, NotAfter: template.NotAfter}
	if err := registry.Stage(context.Background(), binding, bundle); err != nil {
		t.Fatal(err)
	}
	if err := registry.MarkReady(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if err := registry.Activate(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	return registry
}

const (
	handoverNode, handoverEpoch, handoverHost = "edge_handover", "edge_epoch_handover", "machine.runtime.example.test"
)

var handoverIdentity = datacarrier.Identity{AccountID: "account_h", HostID: "host_h", TunnelID: "tunnel_h", ConnectorID: "connector_h", SessionID: "session_h", ProcessGeneration: 1, Generation: 1}

func handoverAdmission(t *testing.T, machinePublic ed25519.PublicKey) runtimewire.Admission {
	digest := sha256.Sum256(machinePublic)
	trust, err := control.NewProcessCarrierServerTrust(handoverNode, handoverEpoch, "127.0.0.1", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return runtimewire.Admission{Schema: runtimewire.Schema, Binding: runtimewire.Binding{
		AccountID: handoverIdentity.AccountID, HostID: handoverIdentity.HostID, TunnelID: handoverIdentity.TunnelID, ConnectorID: handoverIdentity.ConnectorID, SessionID: handoverIdentity.SessionID,
		ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1, RouteID: "route_handover", RouteGeneration: 1,
		EdgeNodeID: handoverNode, EdgeProcessEpoch: handoverEpoch, EdgeCarrierServerSPKISHA256: trust.SPKISHA256, EdgeCarrierServerCertificateChainPEM: trust.CertificateChainPEM,
		MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(machinePublic), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:]),
	}, AttachmentGeneration: 1, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EdgeEndpoints: []string{"h2://127.0.0.1:27443", "h3://127.0.0.1:27444"}, Hostname: handoverHost, RouteKind: runtimewire.RouteKind, RouteRevision: 1, ExpiresAt: time.Now().Add(time.Hour)}
}

// One edge process of TestEdgeHandoverKeepsThePreviewAndTheHostCarrierUp: the real carrier
// component and runtime worker on handover sockets, and a public listener that answers each
// request through the host's carrier (waiting for it like the gateway's pending-preview wait).
func TestEdgeHandoverHelperProcess(t *testing.T) {
	role := os.Getenv("PB_EDGE_HANDOVER_ROLE")
	if role == "" {
		t.Skip("helper process for TestEdgeHandoverKeepsThePreviewAndTheHostCarrierUp")
	}
	dir, tcpAddress, quicAddress, publicAddress := os.Getenv("PB_EDGE_HANDOVER_DIR"), os.Getenv("PB_EDGE_HANDOVER_TCP"), os.Getenv("PB_EDGE_HANDOVER_QUIC"), os.Getenv("PB_EDGE_HANDOVER_PUBLIC")
	machinePublic, err := base64.RawURLEncoding.DecodeString(os.Getenv("PB_EDGE_HANDOVER_MACHINE"))
	if err != nil {
		t.Fatal(err)
	}
	inheritance, err := handover.Inherit(dir)
	if err != nil || (role == "new") != (inheritance != nil) || inheritance != nil && inheritance.Epoch != handoverEpoch {
		t.Fatalf("inherit as %s: %v %v", role, inheritance, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := handoverRegistry(t, role)
	if inheritance != nil {
		// No certificate action reaches this process: it serves only what the serving one handed over.
		report := registry.ImportEncoded(ctx, handoverNode, handoverEpoch, inheritance.Certificates)
		if _, err := registry.GetCertificate(&tls.ClientHelloInfo{ServerName: handoverCertificateHost}); report.Imported != 1 || err != nil {
			t.Fatalf("the replacement lacks the serving process's certificate: %+v %v", report, err)
		}
	}
	source := &missSource{admissions: []runtimewire.Admission{handoverAdmission(t, machinePublic)}}
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: handoverNode, ProcessEpoch: handoverEpoch})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: handoverEpoch})
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: handoverEpoch})
	if err != nil {
		t.Fatal(err)
	}
	worker := &edgeruntime.RuntimeCarrierWorker{Source: source, Expected: expected, Routes: routes, Endpoints: endpoints, Interval: 5 * time.Minute, Timeout: time.Second}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	component, closeService, err := newCarrierComponentWithTelemetry(
		config.Deployment{CarrierTCPListenAddress: tcpAddress, CarrierQUICListenAddress: quicAddress, ControlInterval: time.Hour, NodeCapacity: 8},
		dynamicCarrierCertificate(t, "edge-carrier", nil, nil), nil, nil, nil, nil, nil, nil, worker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if err := component.Start(ctx); err != nil {
		t.Fatal(err)
	}
	public, err := handover.Listen("tcp", publicAddress)
	if err != nil {
		t.Fatal(err)
	}
	var drainer edgeruntime.Drainer
	server := &http.Server{Handler: drainer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if route, ok := routes.Lookup(handoverHost); ok {
				stream, err := route.Server.OpenStream(r.Context(), datacarrier.StreamOpen{
					Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion,
					AccountID: route.Identity.AccountID, TunnelID: route.Identity.TunnelID, ConnectorID: route.Identity.ConnectorID,
					SessionID: route.Identity.SessionID, ProcessGeneration: route.Identity.ProcessGeneration, Generation: route.Identity.Generation,
					RouteID: route.RouteID, RequestID: "request_" + uuid.NewString(), Kind: "http",
				})
				if err == nil {
					_, _ = io.WriteString(stream, "GET\n")
					answer, err := bufio.NewReader(stream).ReadString('\n')
					_ = stream.Close()
					if err == nil {
						_, _ = io.WriteString(w, role+" "+answer)
						return
					}
				}
			}
			if time.Now().After(deadline) {
				http.Error(w, "no host carrier", http.StatusBadGateway)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))}
	go func() { _ = server.Serve(public) }()
	if inheritance != nil {
		select {
		case <-worker.Pulled():
		case <-time.After(10 * time.Second):
			t.Fatal("new edge never pulled its admissions")
		}
		if err := inheritance.Ready(ctx); err != nil {
			t.Fatal(err)
		}
	}
	released := make(chan struct{})
	if _, err := handover.Offer(dir, handoverEpoch, registry.ExportEncoded, func() { close(released) }); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("serving\n")
	if role == "new" {
		<-ctx.Done() // the test kills this process when it is done
		return
	}
	<-released
	drainCtx, stopDrain := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopDrain()
	var drained sync.WaitGroup
	drained.Add(2)
	go func() { defer drained.Done(); _ = drainer.Drain(drainCtx, server, public, 2*time.Second) }()
	go func() {
		defer drained.Done()
		component.(interface {
			Drain(context.Context, time.Time)
		}).Drain(drainCtx, time.Now().Add(3*time.Second))
	}()
	drained.Wait()
	_ = component.Shutdown(drainCtx)
	_ = worker.Shutdown(drainCtx)
	os.Exit(0)
}

// Item 4 of the 2026-11-05 edge-deploy outage: an edge hands over to its replacement while a
// host carrier and an existing preview are in use. Every preview request during the switch is
// answered, and the host is never without a live carrier: it dials the replacement on the edge
// GOAWAY (TCP), keeps serving on the draining one, and the old carrier closes only after.
func TestEdgeHandoverKeepsThePreviewAndTheHostCarrierUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	tcpAddress, quicAddress := dynamicCarrierListenAddresses(t)
	publicAddress, _ := dynamicCarrierListenAddresses(t)
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	start := func(role string) (*exec.Cmd, chan struct{}) {
		command := exec.Command(os.Args[0], "-test.run=^TestEdgeHandoverHelperProcess$")
		command.Env = append(os.Environ(), "PB_EDGE_HANDOVER_ROLE="+role, "PB_EDGE_HANDOVER_DIR="+dir, "PB_EDGE_HANDOVER_TCP="+tcpAddress,
			"PB_EDGE_HANDOVER_QUIC="+quicAddress, "PB_EDGE_HANDOVER_PUBLIC="+publicAddress, "PB_EDGE_HANDOVER_MACHINE="+base64.RawURLEncoding.EncodeToString(machinePublic))
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		serving := make(chan struct{})
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				if scanner.Text() == "serving" {
					close(serving)
				}
			}
		}()
		go func() { _ = command.Wait(); close(exited) }()
		t.Cleanup(func() { _ = command.Process.Kill(); <-exited })
		select {
		case <-serving:
		case <-exited:
			t.Fatalf("%s edge exited before serving", role)
		case <-ctx.Done():
			t.Fatalf("%s edge did not serve", role)
		}
		return command, exited
	}
	_, oldExited := start("old")

	// The host: one carrier, a TCP replacement on the edge GOAWAY, both served until they close.
	certificate, err := dynamicCarrierCertificateWithURI(t, "host-carrier", machinePublic, machinePrivate, handoverIdentity, handoverEpoch)
	if err != nil {
		t.Fatal(err)
	}
	var live atomic.Int32
	var dials atomic.Int32
	var everOffline atomic.Bool
	var dial func()
	serve := func(client *datacarrier.Client) {
		defer func() {
			if live.Add(-1) == 0 {
				everOffline.Store(true)
			}
		}()
		for {
			stream, open, err := client.AcceptStream(ctx)
			if err != nil {
				return
			}
			if open.Kind == connectorprotocol.Goaway {
				notice, err := connectorprotocol.ReadGoawayNotice(stream)
				_ = stream.Close()
				if err == nil && notice.Reason == connectorprotocol.GoawayEdgeDraining {
					go dial()
				}
				continue
			}
			go func() {
				defer stream.Close()
				if _, err := bufio.NewReader(stream).ReadString('\n'); err == nil {
					_, _ = io.WriteString(stream, "ok\n")
				}
			}()
		}
	}
	dial = func() {
		transport := &http2.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
			InsecureSkipVerify: true, // each fixture edge process makes its own server certificate; a real install keeps the claimed one
			NextProtos:         []string{"h2"}}}
		reader, writer := io.Pipe()
		request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+tcpAddress, reader)
		if err != nil {
			t.Error(err)
			return
		}
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Errorf("host carrier dial %d: %v", dials.Load()+1, err)
			return
		}
		config := datacarrier.DefaultConfig()
		config.Identity = handoverIdentity
		config.Authorize = datacarrier.AuthorizerFunc(func(context.Context, datacarrier.Identity, datacarrier.StreamOpen) error { return nil })
		client, err := datacarrier.NewClient(ctx, dynamicHTTPLink{ReadCloser: response.Body, writer: writer}, config)
		if err != nil {
			t.Errorf("host carrier client: %v", err)
			return
		}
		dials.Add(1)
		live.Add(1)
		go serve(client)
	}
	dial()

	// The existing preview, fetched continuously through the whole handover.
	var failures, fromOld, fromNew atomic.Int64
	stop := make(chan struct{})
	var clients sync.WaitGroup
	for range 4 {
		clients.Add(1)
		go func() {
			defer clients.Done()
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				response, err := client.Get("http://" + publicAddress + "/")
				if err != nil {
					failures.Add(1)
					t.Logf("preview request: %v", err)
					continue
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				switch {
				case response.StatusCode != http.StatusOK:
					failures.Add(1)
					t.Logf("preview answered %d: %s", response.StatusCode, body)
				case strings.HasPrefix(string(body), "old "):
					fromOld.Add(1)
				case strings.HasPrefix(string(body), "new "):
					fromNew.Add(1)
				}
			}
		}()
	}
	waitDynamicCondition(t, ctx, func() bool { return fromOld.Load() > 20 })

	start("new")
	select {
	case <-oldExited:
	case <-ctx.Done():
		t.Fatal("old edge did not drain and exit")
	}
	waitDynamicCondition(t, ctx, func() bool { return fromNew.Load() > 20 })
	close(stop)
	clients.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d preview requests failed across the edge handover", failures.Load())
	}
	if everOffline.Load() {
		t.Fatal("the host had no live carrier during the handover")
	}
	if dials.Load() != 2 || live.Load() != 1 {
		t.Fatalf("host carriers: %d dialed, %d live; want the original and one replacement, only the replacement left", dials.Load(), live.Load())
	}
}
