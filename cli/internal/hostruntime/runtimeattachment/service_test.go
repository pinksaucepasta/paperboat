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

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
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

func TestRuntimeAttachmentOutlivesCredentialOutageAndRevokes(t *testing.T) {
	var calls atomic.Int32
	var revoked atomic.Bool
	var document admission
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if revoked.Load() {
			status = http.StatusForbidden
		} else if calls.Add(1) > 1 {
			status = http.StatusServiceUnavailable
		}
		payload, err := json.Marshal(struct {
			Data admission `json:"data"`
		}{document})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload)), Request: request}, nil
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
	expiry := time.Now().Add(2 * time.Second)
	document = admission{Schema: "paperboat.runtime-carrier/v1", RouteKind: "runtime_https_wss", Hostname: "runtime.example.test", EdgeEndpoints: []string{"h2://edge.example.test:8443"}, ExpiresAt: expiry,
		ActiveEdgeNodeID: "edge-test", ActiveEdgeProcessEpoch: "epoch-1234567890123456789012345678",
		Binding: binding{AccountID: "account-test", HostID: "machine-test", TunnelID: "machine-test", ConnectorID: "machine-test", SessionID: "session-test", ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1, RouteID: "route-test", EdgeNodeID: "edge-test", EdgeProcessEpoch: "epoch-1234567890123456789012345678", MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(public), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:]), EdgeCarrierServerSPKISHA256: "sha256:" + hex.EncodeToString(pin[:]), EdgeCarrierServerCertificateChainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: trust.Certificate().Raw}))}}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
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
	var edge *connector.DataCarrier
	select {
	case edge = <-connected:
	case <-ctx.Done():
		t.Fatal("runtime carrier was not admitted")
	}
	defer edge.Close()
	if err := edge.Ping(ctx); err != nil {
		t.Fatal(err)
	}
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
