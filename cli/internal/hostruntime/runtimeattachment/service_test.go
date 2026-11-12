package runtimeattachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/edgeplacement"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeErrorBody struct {
	reader     io.Reader
	closeErr   error
	closeCount atomic.Int32
	closed     chan struct{}
	closeOnce  sync.Once
}

func (body *closeErrorBody) Read(buffer []byte) (int, error) { return body.reader.Read(buffer) }
func (body *closeErrorBody) Close() error {
	body.closeCount.Add(1)
	defer body.closeOnce.Do(func() { close(body.closed) })
	return body.closeErr
}

type readError struct{ err error }

func (reader readError) Read([]byte) (int, error) { return 0, reader.err }

func TestNewUsesOneSharedCarrierWithBrowserStreamCapacity(t *testing.T) {
	service, err := New(Config{
		ControlURL: "https://api.example.test", StateRoot: t.TempDir(), MachineID: "machine-test",
		WorkerGeneration: 1, InstallationGeneration: 1, ListenAddress: "127.0.0.1:38080",
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.sessions == nil {
		t.Fatal("runtime attachment has no shared carrier source")
	}
}

func TestChooseLiveKeepsRecoveredPrimaryAsBackup(t *testing.T) {
	primary := admissionBinding{Binding: binding{EdgeNodeID: "edge-primary", EdgeProcessEpoch: "epoch-primary", SessionID: "session-primary"}}
	backup := admissionBinding{Binding: binding{EdgeNodeID: "edge-backup", EdgeProcessEpoch: "epoch-backup", SessionID: "session-backup"}}
	primaryLive, backupLive := &live{}, &live{}
	ordered := []admissionBinding{primary, backup}
	lives := map[string]*live{carrierKey(primary): primaryLive, carrierKey(backup): backupLive}
	chosen, admission := chooseLive(ordered, lives, backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch)
	if chosen != backupLive || admission.Binding.EdgeNodeID != backup.Binding.EdgeNodeID {
		t.Fatal("recovered primary preempted healthy active backup")
	}
	delete(lives, carrierKey(backup))
	chosen, admission = chooseLive(ordered, lives, backup.Binding.EdgeNodeID, backup.Binding.EdgeProcessEpoch)
	if chosen != primaryLive || admission.Binding.EdgeNodeID != primary.Binding.EdgeNodeID {
		t.Fatal("failed active backup did not promote ready primary")
	}
	backup.Binding.EdgeProcessEpoch = "replacement-epoch"
	ordered[1] = backup
	lives[carrierKey(backup)] = backupLive
	chosen, admission = chooseLive(ordered, lives, "edge-backup", "epoch-backup")
	if chosen != primaryLive || admission.Binding.EdgeNodeID != primary.Binding.EdgeNodeID {
		t.Fatal("stale active edge epoch retained authority")
	}
}

func TestRuntimeCarrierDiagnosticsPreserveAuthorityCauseWithoutText(t *testing.T) {
	secret := errors.New("private machine identity detail")
	err := carrierFailure(errors.Join(preview.ErrMachineAttachmentTrustRequired, secret))
	if !errors.Is(err, preview.ErrMachineAttachmentTrustRequired) || !errors.Is(err, secret) {
		t.Fatalf("typed cause chain=%v", err)
	}
	if strings.Contains(err.Error(), secret.Error()) {
		t.Fatalf("raw identity detail escaped: %v", err)
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "preview_attachment", "lifecycle", "service_failed", err)
	if fault.Stage != peerAuthorityStage || fault.Code != peerAuthorityCode || fault.Cause == "context_canceled" {
		t.Fatalf("authority classification=%+v", fault)
	}
}

func TestStartCanceledDoesNotInstallBackgroundWorker(t *testing.T) {
	service := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error=%v", err)
	}
	if service.done != nil || service.cancel != nil {
		t.Fatal("canceled start installed a background worker")
	}
}

func TestBackgroundFailureObservationsKeepSafeTypedClassesAndRecoveryBoundary(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()

	service := &Service{}
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	secretCause := errors.New("edge credential content")
	first := carrierFailure(errors.Join(preview.ErrMachineAttachmentTrustRequired, secretCause))
	previous := service.observeFailure(ctx, first, nil)
	previous = service.observeFailure(ctx, first, previous)
	previous = service.observeFailure(ctx, peerConnectFailure(secretCause), previous)
	previous = service.observeFailure(ctx, errors.Join(context.Canceled, secretCause), previous)
	previous = service.observeFailure(ctx, context.Canceled, previous)

	if len(faults) != 3 {
		t.Fatalf("faults after duplicate and cancellation filtering=%d, want 3", len(faults))
	}
	if faults[0].Stage != peerAuthorityStage || faults[0].Code != peerAuthorityCode || faults[0].SupportReference != reference {
		t.Fatalf("authority fault=%+v", faults[0])
	}
	if faults[1].Stage != peerConnectStage || faults[1].Code != transportFailure {
		t.Fatalf("connect fault=%+v", faults[1])
	}
	if faults[2].Stage != "lifecycle" || faults[2].Code != "service_failed" || faults[2].Cause == "context_canceled" {
		t.Fatalf("mixed cancellation fault=%+v", faults[2])
	}
	for _, fault := range faults {
		if strings.Contains(strings.Join(fault.ErrorChain, " "), "edge credential content") || fault.SupportReference != reference {
			t.Fatalf("unsafe or uncorrelated fault=%+v", fault)
		}
	}
}

func TestStartReportsControlResponseFailuresAfterClosingBody(t *testing.T) {
	now := time.Now().UTC()
	payload, err := json.Marshal(struct {
		Data admission `json:"data"`
	}{Data: admission{
		Schema:    "paperboat.runtime-carrier/v1",
		Binding:   binding{HostID: "machine-test", SessionID: "session-test", RouteID: "route-test", ProcessGeneration: 1, InstallationGeneration: 1},
		RouteKind: "runtime_https_wss", ExpiresAt: now.Add(time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		status      int
		reader      io.Reader
		closeErr    error
		privateText string
		wantStatus  int
	}{
		{
			name: "successful response close failure", status: http.StatusOK, reader: bytes.NewReader(payload),
			closeErr: errors.Join(errors.New("private close detail"), syscall.EIO), privateText: "private close detail",
		},
		{
			name: "rejected response drain failure", status: http.StatusServiceUnavailable,
			reader: readError{err: errors.Join(errors.New("private drain detail"), syscall.EIO)}, privateText: "private drain detail", wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &closeErrorBody{reader: test.reader, closeErr: test.closeErr, closed: make(chan struct{})}
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != path {
					return nil, errors.New("unexpected control request")
				}
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: body, Request: request}, nil
			})
			service := newRuntimeAttachmentTestService(t, transport)
			faults := make(chan errorreport.Fault, 2)
			restored := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
				select {
				case <-body.closed:
				default:
					t.Error("failure was observed before the response body closed")
				}
				faults <- fault
			})
			defer restored()
			ctx, cancel := context.WithCancel(context.Background())
			if err := service.Start(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			var stopOnce sync.Once
			stop := func() {
				stopOnce.Do(func() {
					cancel()
					stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
					defer stopCancel()
					if err := service.Shutdown(stopCtx); err != nil {
						t.Errorf("shutdown after response failure: %v", err)
					}
				})
			}
			t.Cleanup(stop)
			select {
			case fault := <-faults:
				if fault.Stage != controlRequestStage || fault.Code != controlRequestCode || fault.Errno != int(syscall.EIO) || fault.HTTPStatus != test.wantStatus || !supportref.Valid(fault.SupportReference) {
					t.Fatalf("response failure fault=%+v", fault)
				}
				if strings.Contains(fmt.Sprintf("%+v", fault), test.privateText) {
					t.Fatalf("private response detail escaped diagnostic metadata: %+v", fault)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("response failure was not reported")
			}
			stop()
			if body.closeCount.Load() != 1 {
				t.Fatalf("response body close count=%d, want 1", body.closeCount.Load())
			}
			select {
			case duplicate := <-faults:
				t.Fatalf("response failure reported more than once: %+v", duplicate)
			default:
			}
		})
	}
}

func newRuntimeAttachmentTestService(t *testing.T, transport http.RoundTripper) *Service {
	t.Helper()
	root := t.TempDir()
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	now := time.Now().UTC()
	if err := store.SaveRegistration(identity.Registration{
		ServerURL: "https://control.example", MachineID: "machine-test", EnvironmentID: "environment-test",
		PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()),
		InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMachineControl(identity.MachineControl{
		MachineID: "machine-test", EnvironmentID: "environment-test", InstallationGeneration: 1,
		Credential: strings.Repeat("c", 32), ExpiresAt: now.Add(time.Hour), KeyID: key.ID,
	}); err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{
		ControlURL: "https://control.example", StateRoot: root, Transport: transport, MachineID: "machine-test",
		WorkerGeneration: 1, InstallationGeneration: 1, ListenAddress: "127.0.0.1:38080",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// startConnectedRuntimeCarrier runs the service against a control plane whose attachment response
// status is status() and whose admission expires at expiry, and returns the admitted edge carrier.
func startConnectedRuntimeCarrier(t *testing.T, ctx context.Context, expiry time.Time, status func() int) (*Service, *connector.DataCarrier) {
	t.Helper()
	return startConnectedRuntimeCarrierWith(t, ctx, expiry, status, nil, nil)
}

// startConnectedRuntimeCarrierWith also lets tweak change the attachment answer and other answer
// every other control path.
func startConnectedRuntimeCarrierWith(t *testing.T, ctx context.Context, expiry time.Time, status func() int, tweak func(*admission), other http.RoundTripper) (*Service, *connector.DataCarrier) {
	t.Helper()
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{expiry: expiry, status: status, tweak: tweak, other: other})
	select {
	case dial := <-h.edges:
		if err := dial.edge.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		return h.service, dial.edge
	case <-ctx.Done():
		t.Fatal("runtime carrier was not admitted")
	}
	return nil, nil
}

type runtimeCarrierOptions struct {
	expiry time.Time
	status func() int
	tweak  func(*admission)
	other  http.RoundTripper
	// confirmActive makes the control plane take the active edge the host asks for.
	confirmActive bool
	// failDial fails a carrier dial to edgeID (dial counts that edge's dials from 1).
	failDial func(edgeID string, dial int) bool
}

type runtimeCarrierHarness struct {
	service *Service
	// edges receives the edge end of each carrier the host dials, in order.
	edges chan edgeDial
	// actives receives each active edge node the host asks the control plane for.
	actives chan string
}

type edgeDial struct {
	edgeID string
	edge   *connector.DataCarrier
}

// startRuntimeCarrier runs the service against a control plane whose attachment response status is
// status() and whose admission expires at expiry. Every carrier dial reaches a new edge carrier.
func startRuntimeCarrier(t *testing.T, ctx context.Context, options runtimeCarrierOptions) *runtimeCarrierHarness {
	t.Helper()
	h := &runtimeCarrierHarness{edges: make(chan edgeDial, 16), actives: make(chan string, 64)}
	var mu sync.Mutex
	var document admission
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if options.other != nil && request.URL.Path != path {
			return options.other.RoundTrip(request)
		}
		mu.Lock()
		answer := document
		mu.Unlock()
		if options.confirmActive && request.Body != nil {
			var asked struct {
				Node  string `json:"active_edge_node_id"`
				Epoch string `json:"active_edge_process_epoch"`
			}
			if json.NewDecoder(request.Body).Decode(&asked) == nil && asked.Node != "" {
				mu.Lock()
				document.ActiveEdgeNodeID, document.ActiveEdgeProcessEpoch = asked.Node, asked.Epoch
				answer = document
				mu.Unlock()
				select {
				case h.actives <- asked.Node:
				default:
				}
			}
		}
		payload, err := json.Marshal(struct {
			Data admission `json:"data"`
		}{answer})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: options.status(), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload)), Request: request}, nil
	})
	service := newRuntimeAttachmentTestService(t, transport)
	h.service = service
	store, err := identity.Open(identity.Config{StateRoot: service.config.StateRoot})
	if err != nil {
		t.Fatal(err)
	}
	public := store.Current().Public()
	digest := sha256.Sum256(public)
	trust := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(trust.Close)
	pin := sha256.Sum256(trust.Certificate().RawSubjectPublicKeyInfo)
	document = admission{Schema: "paperboat.runtime-carrier/v1", RouteKind: "runtime_https_wss", Hostname: "runtime.example.test", EdgeEndpoints: []string{"h2://edge.example.test:8443"}, ExpiresAt: options.expiry,
		ActiveEdgeNodeID: "edge-test", ActiveEdgeProcessEpoch: "epoch-1234567890123456789012345678",
		Binding: binding{AccountID: "account-test", HostID: "machine-test", TunnelID: "machine-test", ConnectorID: "machine-test", SessionID: "session-test", ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1, RouteID: "route-test", EdgeNodeID: "edge-test", EdgeProcessEpoch: "epoch-1234567890123456789012345678", MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(public), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:]), EdgeCarrierServerSPKISHA256: "sha256:" + hex.EncodeToString(pin[:]), EdgeCarrierServerCertificateChainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trust.Certificate().Raw}))}}
	if options.tweak != nil {
		options.tweak(&document)
	}
	dials := map[string]int{}
	source, err := preview.NewMachineAttachmentSessionSource(preview.MachineAttachmentSessionSourceConfig{StateRoot: service.config.StateRoot, SessionFactory: func(got connector.DataCarrierIdentity, config connector.DataCarrierPoolConfig, _ connector.NetworkDialerConfig) (connector.DataCarrierSessionSource, error) {
		return connector.NewDataCarrierSessionSource(got, config, func(_ context.Context, request connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
			mu.Lock()
			dials[request.EdgeID]++
			dial := dials[request.EdgeID]
			mu.Unlock()
			if options.failDial != nil && options.failDial(request.EdgeID, dial) {
				return connector.DataCarrierDialResult{}, &connector.TransportDialError{Transport: request.Transport, Err: errors.New("edge refused"), Fallback: false}
			}
			local, remote := net.Pipe()
			edge, err := connector.NewDataCarrierServer(ctx, remote, config.Carrier, connector.DataCarrierAdmission{Identity: got, Authorize: func(context.Context, connector.StreamOpen) error { return nil }})
			if err != nil {
				local.Close()
				remote.Close()
				return connector.DataCarrierDialResult{}, err
			}
			t.Cleanup(func() { edge.Close() })
			select {
			case h.edges <- edgeDial{edgeID: request.EdgeID, edge: edge}:
			default:
			}
			return connector.DataCarrierDialResult{Link: local, PeerIdentity: got, Transport: request.Transport, EdgeID: request.EdgeID, FailureDomain: request.FailureDomain}, nil
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.sessions = source
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return h
}

func TestRuntimeAttachmentOutlivesCredentialOutageAndRevokes(t *testing.T) {
	confirmRevocationAfter(t, 300*time.Millisecond)
	var calls atomic.Int32
	var revoked atomic.Bool
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	expiry := time.Now().Add(2 * time.Second)
	_, edge := startConnectedRuntimeCarrier(t, ctx, expiry, func() int {
		if revoked.Load() {
			return http.StatusForbidden
		} else if calls.Add(1) > 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	select {
	case <-edge.Done():
		t.Fatal("credential expiry/outage closed admitted runtime carrier")
	case <-time.After(time.Until(expiry) + 100*time.Millisecond):
	}
	if err := edge.Ping(ctx); err != nil {
		t.Fatal("runtime carrier unusable after expiry", err)
	}
	revoked.Store(true)
	select {
	case <-edge.Done():
	case <-ctx.Done():
		t.Fatal("explicit runtime revocation did not close carrier")
	}
}

func confirmRevocationAfter(t *testing.T, d time.Duration) {
	t.Helper()
	old := revocationConfirmation
	revocationConfirmation = d
	t.Cleanup(func() { revocationConfirmation = old })
}

// A server swap or rollback refuses a valid machine for a while (2026-11-12 canary-b: the attachment ended at the
// first 401 and never came back). Refusals shorter than the confirmation keep the carrier and the attachment
// recovers when the server answers again.
func TestRuntimeAttachmentSurvivesRefusalsDuringAServerSwap(t *testing.T) {
	confirmRevocationAfter(t, 5*time.Second)
	var refuse atomic.Bool
	var refused atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	// The short admission makes the host ask again within the test; a healthy long one is rechecked only every 30 s.
	expiry := time.Now().Add(2500 * time.Millisecond)
	_, edge := startConnectedRuntimeCarrier(t, ctx, expiry, func() int {
		if refuse.Load() {
			refused.Add(1)
			return http.StatusUnauthorized
		}
		return http.StatusOK
	})
	refuse.Store(true)
	select {
	case <-edge.Done():
		t.Fatal("a short refusal ended the attachment")
	case <-time.After(2 * time.Second):
	}
	refuse.Store(false)
	if refused.Load() == 0 {
		t.Fatal("the host never asked while the server refused")
	}
	if err := edge.Ping(ctx); err != nil {
		t.Fatal("carrier unusable after the refusals", err)
	}
}

// The control plane fails (5xx) while the carrier drops, as in a server rollback: the host keeps asking with bounded
// backoff and attaches again once the server answers, with no daemon restart (2026-11-12 canary-b).
func TestRuntimeAttachmentReattachesAfterServerErrorsWithoutRestart(t *testing.T) {
	var fail atomic.Bool
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := startRuntimeCarrier(t, ctx, runtimeCarrierOptions{expiry: time.Now().Add(time.Hour), status: func() int {
		if fail.Load() {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}})
	first := nextEdge(t, h, 5*time.Second)
	fail.Store(true)
	first.edge.Close()
	time.Sleep(3 * time.Second)
	select {
	case <-h.edges:
		t.Fatal("host dialed a carrier while the control plane was failing")
	default:
	}
	fail.Store(false)
	again := nextEdge(t, h, 15*time.Second)
	if err := again.edge.Ping(ctx); err != nil {
		t.Fatal("re-attached carrier unusable", err)
	}
}

// Every machine POSTed its carrier attachment once per second even while healthy (2026-10-22: ~21/s
// fleet-wide, the main writer of runtime_carrier_attachments during the storm). A healthy carrier
// with a long admission asks again only after healthyRecheck.
func TestHealthyRuntimeCarrierDoesNotAskForItsAttachmentEverySecond(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, edge := startConnectedRuntimeCarrier(t, ctx, time.Now().Add(time.Hour), func() int { calls.Add(1); return http.StatusOK })
	settled := calls.Load()
	time.Sleep(3500 * time.Millisecond)
	if extra := calls.Load() - settled; extra != 0 {
		t.Fatalf("healthy carrier made %d attachment requests in 3.5 s", extra)
	}
	if err := edge.Ping(ctx); err != nil {
		t.Fatal("carrier unusable", err)
	}
}

func TestRoundPacerRechecksHealthyCarriersAndRunsAtOnceWhenOneDrops(t *testing.T) {
	now := time.Now()
	var pacer roundPacer
	if !pacer.due(now, false) {
		t.Fatal("first round must run")
	}
	pacer.finished(now, true, now.Add(time.Hour))
	if pacer.due(now.Add(time.Second), true) || !pacer.due(now.Add(healthyRecheck), true) {
		t.Fatal("healthy carriers must be rechecked after healthyRecheck, not every tick")
	}
	if !pacer.due(now.Add(time.Second), false) {
		t.Fatal("a carrier that is no longer ready must run a round at once")
	}
	pacer.finished(now, true, now.Add(10*time.Second))
	if pacer.due(now.Add(4*time.Second), true) || !pacer.due(now.Add(5*time.Second), true) {
		t.Fatal("a short admission must be rechecked at half its remaining life")
	}
	pacer.finished(now, false, now.Add(time.Hour))
	if !pacer.due(now.Add(time.Millisecond), true) {
		t.Fatal("after an unhealthy round every tick runs (the admission poll paces failures)")
	}
}

// Edge restart (12:30Z incident): one placement candidate is unresponsive while
// another is healthy. The unresponsive probe must not keep the healthy edge
// from being admitted.
func TestRuntimeCarrierConnectsWhileAnotherEdgeCandidateIsUnresponsive(t *testing.T) {
	var document admission
	var requestsMu sync.Mutex
	var reportedLatency []edgeLatency
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Body != nil {
			raw, _ := io.ReadAll(request.Body)
			request.Body = io.NopCloser(bytes.NewReader(raw))
			var sent struct {
				EdgeLatency []edgeLatency `json:"edge_latency"`
			}
			if json.Unmarshal(raw, &sent) == nil && len(sent.EdgeLatency) != 0 {
				requestsMu.Lock()
				reportedLatency = append(reportedLatency, sent.EdgeLatency...)
				requestsMu.Unlock()
			}
		}
		payload, err := json.Marshal(struct {
			Data admission `json:"data"`
		}{document})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload)), Request: request}, nil
	})
	service := newRuntimeAttachmentTestService(t, transport)
	store, err := identity.Open(identity.Config{StateRoot: service.config.StateRoot})
	if err != nil {
		t.Fatal(err)
	}
	public := store.Current().Public()
	digest := sha256.Sum256(public)
	trust := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer trust.Close()
	pin := sha256.Sum256(trust.Certificate().RawSubjectPublicKeyInfo)
	const epoch = "epoch-1234567890123456789012345678"
	document = admission{Schema: "paperboat.runtime-carrier/v1", RouteKind: "runtime_https_wss", Hostname: "runtime.example.test", EdgeEndpoints: []string{"h2://edge.example.test:8443"}, ExpiresAt: time.Now().Add(time.Hour),
		ActiveEdgeNodeID: "edge-test", ActiveEdgeProcessEpoch: epoch,
		EdgeCandidates: []edgeplacement.Candidate{
			{NodeID: "edge-test", ProcessEpoch: epoch, Region: "region-a", FailureDomain: "domain-a", ProbeAddress: "127.0.0.1:38080"},
			{NodeID: "edge-restarting", ProcessEpoch: "epoch-restarting-12345678901234567", Region: "region-b", FailureDomain: "domain-b", ProbeAddress: "127.0.0.1:38081"},
		},
		Binding: binding{AccountID: "account-test", HostID: "machine-test", TunnelID: "machine-test", ConnectorID: "machine-test", SessionID: "session-test", ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1, RouteID: "route-test", EdgeNodeID: "edge-test", EdgeProcessEpoch: epoch, MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(public), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:]), EdgeCarrierServerSPKISHA256: "sha256:" + hex.EncodeToString(pin[:]), EdgeCarrierServerCertificateChainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trust.Certificate().Raw}))}}
	// The restarting edge accepts no connections and sends no reset: its probe
	// only ends when the probe's own deadline does.
	service.probeDial = func(ctx context.Context, address string) error {
		if address == "127.0.0.1:38081" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	connected := make(chan *connector.DataCarrier, 1)
	source, err := preview.NewMachineAttachmentSessionSource(preview.MachineAttachmentSessionSourceConfig{StateRoot: service.config.StateRoot, SessionFactory: func(got connector.DataCarrierIdentity, config connector.DataCarrierPoolConfig, _ connector.NetworkDialerConfig) (connector.DataCarrierSessionSource, error) {
		local, remote := net.Pipe()
		edge, err := connector.NewDataCarrierServer(ctx, remote, config.Carrier, connector.DataCarrierAdmission{Identity: got, Authorize: func(context.Context, connector.StreamOpen) error { return nil }})
		if err != nil {
			local.Close()
			remote.Close()
			return connector.DataCarrierSessionSource{}, err
		}
		connected <- edge
		return connector.NewDataCarrierSessionSource(got, config, func(_ context.Context, request connector.DataCarrierDialRequest) (connector.DataCarrierDialResult, error) {
			return connector.DataCarrierDialResult{Link: local, PeerIdentity: got, Transport: request.Transport, EdgeID: request.EdgeID, FailureDomain: request.FailureDomain}, nil
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.sessions = source
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = service.Shutdown(context.Background()) }()
	select {
	case edge := <-connected:
		defer edge.Close()
	case <-time.After(8 * time.Second):
		t.Fatal("runtime carrier was not admitted while another edge candidate was unresponsive")
	}
	// The healthy edge's probe time is reported to the server; the unresponsive one has no measurement.
	deadline := time.Now().Add(8 * time.Second)
	for {
		requestsMu.Lock()
		reported := append([]edgeLatency(nil), reportedLatency...)
		requestsMu.Unlock()
		if len(reported) != 0 {
			for _, sample := range reported {
				if sample.NodeID != "edge-test" || sample.ProcessEpoch != epoch || sample.RTTMilliseconds < 1 || sample.RTTMilliseconds > maximumLatencyMilliseconds {
					t.Fatalf("unexpected latency report %+v", sample)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe time of the healthy edge was never reported to the server")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// PLAN 3a: the agent advertises that it serves endpoint routes on its runtime carriers, and uses
// the engine for previews only when the server says every edge of the attachment admits them.
func TestRuntimeAttachmentAdvertisesEndpointsAndReadsWhetherEdgesServeThem(t *testing.T) {
	var sent struct {
		Capabilities []string `json:"capabilities"`
	}
	served := false
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatal(err)
		}
		document := admission{Schema: "paperboat.runtime-carrier/v1", RouteKind: "runtime_https_wss", ExpiresAt: time.Now().Add(time.Hour), EndpointsServed: served,
			Binding: binding{HostID: "machine-test", InstallationGeneration: 1, ProcessGeneration: 1, RouteID: "route-test", SessionID: "session-test"}}
		payload, _ := json.Marshal(struct {
			Data admission `json:"data"`
		}{document})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload)), Request: request}, nil
	})
	service := newRuntimeAttachmentTestService(t, transport)
	for _, want := range []bool{false, true} {
		served = want
		out, err := service.fetch(t.Context(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(sent.Capabilities) != 2 || sent.Capabilities[0] != "endpoints" || sent.Capabilities[1] != "tunnel_endpoints" {
			t.Fatalf("attachment request capabilities = %v", sent.Capabilities)
		}
		if out.EndpointsServed != want {
			t.Fatalf("endpoints_served = %v, want %v", out.EndpointsServed, want)
		}
	}
}

// F7-04: a background preview resumed right after a host restart must not choose its carrier
// before the agent's first attachment answer says whether the edges serve endpoint routes.
func TestEndpointsServedWaitsForTheFirstAttachmentAnswer(t *testing.T) {
	service := newRuntimeAttachmentTestService(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}))
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if service.EndpointsServed(short) {
		t.Fatal("served reported before any attachment answer")
	}
	result := make(chan bool, 1)
	go func() { result <- service.EndpointsServed(t.Context()) }()
	time.Sleep(50 * time.Millisecond)
	service.hubsMu.Lock()
	service.served = true
	service.hubsMu.Unlock()
	service.answeredOnce.Do(func() { close(service.answered) })
	select {
	case served := <-result:
		if !served {
			t.Fatal("waiting caller got false after the answer said served")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting caller not released by the first answer")
	}
}

// Audit T10: a daemon closing its runtime carrier first tells the edge it is going away, so the
// edge routes nothing new to a carrier that is closing.
func TestClosingRuntimeCarrierSendsHostGoaway(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	service, edge := startConnectedRuntimeCarrier(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK })
	deadline := time.Now().Add(5 * time.Second)
	for hubs, _ := service.Hubs(); len(hubs) == 0; hubs, _ = service.Hubs() {
		if time.Now().After(deadline) {
			t.Fatal("runtime carrier never came into service")
		}
		time.Sleep(20 * time.Millisecond)
	}
	go func() { _ = service.Shutdown(context.Background()) }()
	for {
		stream, open, err := edge.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("no goaway before the carrier closed: %v", err)
		}
		if open.Kind != connectorprotocol.Goaway {
			_ = stream.Close()
			continue
		}
		notice, err := connectorprotocol.ReadGoawayNotice(stream)
		_ = stream.Close()
		if err != nil || notice.Reason != connectorprotocol.GoawayHostDraining || notice.DrainDeadline < time.Now().Unix() || open.RouteID != "route-test" {
			t.Fatalf("goaway = %+v %+v, %v", open, notice, err)
		}
		return
	}
}

// pbvm4 feat9: the old worker closed its carriers right after its goaway while the edge still
// sent it the preview's requests. Make-before-break: after the goaway the worker keeps serving
// as long as the edge opens streams on its carrier, and closes once the edge has moved to the
// next worker's carrier (handoverQuiet without a stream), never later than carrierDrainGrace.
func TestGoawayCarrierServesUntilTheEdgeStopsOpeningStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	service, edge := startConnectedRuntimeCarrier(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if hubs, _ := service.Hubs(); len(hubs) != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime carrier never came into service")
		}
	}
	shutdown := make(chan struct{})
	go func() { _ = service.Shutdown(context.Background()); close(shutdown) }()
	var request connector.StreamOpen
	for {
		stream, open, err := edge.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("no goaway: %v", err)
		}
		_ = stream.Close()
		if open.Kind == connectorprotocol.Goaway {
			request = open
			break
		}
	}
	// The edge has not moved yet: it keeps sending requests for longer than handoverQuiet.
	request.Kind = "http"
	until := time.Now().Add(handoverQuiet + time.Second)
	for index := 0; time.Now().Before(until); index++ {
		request.RequestID = fmt.Sprintf("request-%d", index)
		stream, err := edge.OpenStream(ctx)
		if err == nil {
			err = connectorprotocol.WriteStreamOpen(stream, request)
			_ = stream.Close()
		}
		if err != nil {
			t.Fatalf("the draining worker stopped serving while the edge still sent it requests: %v", err)
		}
		select {
		case <-shutdown:
			t.Fatal("the worker closed while the edge still sent it requests")
		case <-time.After(200 * time.Millisecond):
		}
	}
	moved := time.Now()
	select {
	case <-shutdown:
		if waited := time.Since(moved); waited < handoverQuiet-200*time.Millisecond {
			t.Fatalf("closed %v after the last request, want about %v", waited, handoverQuiet)
		}
	case <-time.After(carrierDrainGrace):
		t.Fatal("the worker did not close after the edge stopped sending it requests")
	}
}

// pb update: after the handover the retiring worker keeps its carrier while it serves browser
// terminals, and only until the last one leaves or terminalDrainLimit; Shutdown then closes at once.
func TestRetiringWorkerKeepsItsCarrierForHeldBrowserTerminals(t *testing.T) {
	for _, end := range []string{"last terminal leaves", "drain limit"} {
		t.Run(end, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			service, edge := startConnectedRuntimeCarrier(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK })
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if hubs, _ := service.Hubs(); len(hubs) != 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("runtime carrier never came into service")
				}
			}
			var attached atomic.Int32
			attached.Store(1)
			service.attached = func() int { return int(attached.Load()) }
			if end == "drain limit" {
				previous := terminalDrainLimit
				terminalDrainLimit = time.Second
				t.Cleanup(func() { terminalDrainLimit = previous })
			}
			started := time.Now()
			drained := make(chan error, 1)
			go func() { drained <- service.DrainBrowserTerminals(ctx) }()
			var request connector.StreamOpen
			for {
				stream, open, err := edge.AcceptStream(ctx)
				if err != nil {
					t.Fatalf("no goaway: %v", err)
				}
				_ = stream.Close()
				if open.Kind == connectorprotocol.Goaway {
					request = open
					break
				}
			}
			if end == "drain limit" {
				select {
				case err := <-drained:
					if err != nil || time.Since(started) < handoverQuiet+time.Second-100*time.Millisecond {
						t.Fatalf("drain ended after %v (%v), want the handover then the limit", time.Since(started), err)
					}
				case <-time.After(handoverQuiet + 3*time.Second):
					t.Fatal("the drain outlived its limit")
				}
			} else {
				// Past the handover the carrier still serves the held terminal's streams.
				time.Sleep(handoverQuiet + 500*time.Millisecond)
				request.Kind, request.RequestID = "http", "request-held"
				stream, err := edge.OpenStream(ctx)
				if err == nil {
					err = connectorprotocol.WriteStreamOpen(stream, request)
					_ = stream.Close()
				}
				if err != nil {
					t.Fatalf("the retiring worker closed its carrier under a held terminal: %v", err)
				}
				select {
				case <-drained:
					t.Fatal("the drain ended while a browser terminal was held")
				default:
				}
				attached.Store(0)
				select {
				case err := <-drained:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("the drain did not end when the last terminal left")
				}
			}
			closing := time.Now()
			if err := service.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			// No second goaway: Shutdown waits at most until the edge has been quiet for handoverQuiet.
			if waited := time.Since(closing); waited >= handoverQuiet+500*time.Millisecond {
				t.Fatalf("Shutdown waited %v, want at most handoverQuiet after the last request", waited)
			}
		})
	}
}

// pbvm1 vm1-1a6479a4-124638: the held browser terminal ended at 12:51:13.7, so the drain returned and
// Shutdown closed the old carriers at once, while the edge still had the tunnel route on the old carrier
// until 20.86: a request hung 5 s and failed. Shutdown keeps serving until the edge has opened no stream
// for handoverQuiet, never longer than carrierDrainGrace.
func TestShutdownAfterTheHandoverServesWhileTheEdgeStillSendsRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	service, edge := startConnectedRuntimeCarrier(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if hubs, _ := service.Hubs(); len(hubs) != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime carrier never came into service")
		}
	}
	service.attached = func() int { return 0 }
	drained := make(chan error, 1)
	go func() { drained <- service.DrainBrowserTerminals(ctx) }()
	var request connector.StreamOpen
	for {
		stream, open, err := edge.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("no goaway: %v", err)
		}
		_ = stream.Close()
		if open.Kind == connectorprotocol.Goaway {
			request = open
			break
		}
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	request.Kind = "http"
	send := func(index int) error {
		request.RequestID = fmt.Sprintf("request-%d", index)
		stream, err := edge.OpenStream(ctx)
		if err == nil {
			err = connectorprotocol.WriteStreamOpen(stream, request)
			_ = stream.Close()
		}
		return err
	}
	for index := range 3 {
		if err := send(index); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	shutdown := make(chan struct{})
	go func() { _ = service.Shutdown(context.Background()); close(shutdown) }()
	// The edge still holds a route on this carrier and keeps sending it requests.
	until := time.Now().Add(handoverQuiet + time.Second)
	for index := 3; time.Now().Before(until); index++ {
		select {
		case <-shutdown:
			t.Fatal("Shutdown closed the carrier while the edge still sent it requests")
		case <-time.After(200 * time.Millisecond):
		}
		if err := send(index); err != nil {
			t.Fatalf("the retiring worker stopped serving while the edge still sent it requests: %v", err)
		}
	}
	select {
	case <-shutdown:
	case <-time.After(handoverQuiet + time.Second):
		t.Fatal("Shutdown did not close after the edge stopped sending it requests")
	}
}

// pbvm4 update check: a browser terminal held by the retiring worker stayed attached to silence for 19-25 s (the
// page never left "attached"): the drain waited for it to leave while the worker could no longer serve it. After the
// goaways the worker ends the terminals it holds, so the edge moves each browser to the successor at once.
func TestDrainEndsTheBrowserTerminalsItHoldsAfterTheHandover(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	service, edge := startConnectedRuntimeCarrier(t, ctx, time.Now().Add(time.Hour), func() int { return http.StatusOK })
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if hubs, _ := service.Hubs(); len(hubs) != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime carrier never came into service")
		}
	}
	var held atomic.Int32
	held.Store(1)
	service.attached = func() int { return int(held.Load()) }
	sawGoaway := make(chan struct{})
	service.OnTerminalHandover(func() {
		select {
		case <-sawGoaway:
		default:
			t.Error("the terminals were ended before the edges heard the goaway")
		}
		held.Store(0)
	})
	drained := make(chan error, 1)
	go func() { drained <- service.DrainBrowserTerminals(ctx) }()
	for {
		stream, open, err := edge.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("no goaway: %v", err)
		}
		_ = stream.Close()
		if open.Kind == connectorprotocol.Goaway {
			close(sawGoaway)
			break
		}
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the drain waited for a browser terminal that the worker could no longer serve")
	}
	if held.Load() != 0 {
		t.Fatal("the terminals were not ended")
	}
}
