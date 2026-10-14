package runtimeattachment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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
