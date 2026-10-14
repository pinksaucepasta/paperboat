package configsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type credentialSyncer struct {
	client *ControlClient
	values []string
	errors []error
}

type cancelingSyncer struct {
	cancel context.CancelFunc
	cause  error
}

func (s cancelingSyncer) Sync(context.Context, string) (PublishResult, error) {
	s.cancel()
	return PublishResult{}, s.cause
}

func (s *credentialSyncer) Sync(ctx context.Context, _ string) (PublishResult, error) {
	credential, err := s.client.Credential(ctx)
	if err != nil {
		s.errors = append(s.errors, err)
		return PublishResult{}, err
	}
	s.values = append(s.values, credential.Value)
	return PublishResult{RemoteRevision: "head", Landed: true}, nil
}

func TestCredentialOutageRecoversWithoutRevokingAssignment(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/config/credentials" {
			http.NotFound(w, r)
			return
		}
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "private credential response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"credential": "stable-credential", "environment_id": "environment", "machine_id": "helper",
			"assignment_id": "assignment", "assignment_version": 1, "warning_revision": "warning",
			"expires_at": now.Add(4 * time.Minute),
		}})
	}))
	defer server.Close()

	client, err := NewControlClient(ControlClientConfig{
		BaseURL: server.URL, AllowedHosts: []string{"127.0.0.1"},
		Identities: tokenSourceFunc(func(context.Context) (string, error) { return "identity", nil }),
		Proofs:     &recordingProofSource{}, OperationID: func() (string, error) { return "operation", nil },
		Transport: server.Client().Transport, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	syncer := &credentialSyncer{client: client}
	engine, err := NewEngine(EngineConfig{HomeRoot: resolvedTempDir(t), Descriptor: testEngineDescriptor(1), Syncer: syncer})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	recorder := diagnostics.NewMemoryRecorder()
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), recorder)

	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()

	engine.backgroundSync(ctx)
	if engine.status.State == "revoked" || engine.status.ErrorCode == "credential_expired" {
		t.Fatalf("temporary service outage revoked the assignment: %#v", engine.status)
	}
	if len(syncer.errors) != 1 || errors.Is(syncer.errors[0], ErrAuthorization) || strings.Contains(syncer.errors[0].Error(), "private credential response") {
		t.Fatalf("outage cause=%v", syncer.errors)
	}
	engine.backgroundSync(ctx)
	if engine.status.State != "healthy" || requests != 2 || len(syncer.values) != 1 || syncer.values[0] != "stable-credential" {
		t.Fatalf("recovery state=%#v requests=%d credentials=%#v", engine.status, requests, syncer.values)
	}

	var outage errorreport.Fault
	for _, fault := range faults {
		if fault.HTTPStatus == http.StatusServiceUnavailable && fault.Code == "control_request_failed" {
			outage = fault
			break
		}
	}
	if outage.Code == "" || outage.SupportReference != reference || outage.Stage != "control_request" || outage.Outcome != "failed" {
		t.Fatalf("outage fault = %#v", outage)
	}
	if strings.Contains(strings.Join(outage.ErrorChain, ","), "private") {
		t.Fatalf("private response content entered fault: %#v", outage)
	}
	recovered := 0
	for _, event := range recorder.Recent() {
		if event.Code == "recovered" {
			recovered++
			if event.SupportReference != outage.SupportReference {
				t.Fatalf("recovery reference=%q, outage reference=%q", event.SupportReference, outage.SupportReference)
			}
		}
	}
	if recovered != 1 {
		t.Fatalf("recovery events=%d, want exactly one", recovered)
	}
}

func TestCredentialAuthorizationDenialRequiresPureHTTPRejection(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		response string
		closeErr error
		want     bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, want: true},
		{name: "forbidden", status: http.StatusForbidden, want: true},
		{name: "unauthorized with body close failure", status: http.StatusUnauthorized, closeErr: syscall.EIO, want: false},
		{name: "service unavailable", status: http.StatusServiceUnavailable, want: false},
		{name: "malformed success response", status: http.StatusOK, response: "not json", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewControlClient(ControlClientConfig{
				BaseURL: "https://control.example.test", AllowedHosts: []string{"control.example.test"},
				Identities: tokenSourceFunc(func(context.Context) (string, error) { return "identity", nil }),
				Proofs:     &recordingProofSource{}, OperationID: func() (string, error) { return "operation", nil },
				Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					body := io.NopCloser(strings.NewReader(test.response))
					if test.closeErr != nil {
						body = failingCloseBody{closeErr: test.closeErr}
					}
					return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: body, Request: req}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Credential(context.Background())
			if got := expectedAuthorizationDenial(err); got != test.want {
				t.Fatalf("expected authorization denial=%t, error=%v", got, err)
			}
			if test.closeErr != nil && !errors.Is(err, test.closeErr) {
				t.Fatalf("body close cause lost: %v", err)
			}
			if (test.status == http.StatusServiceUnavailable || test.status == http.StatusOK) && errors.Is(err, ErrAuthorization) {
				t.Fatalf("operational failure classified as authorization: %v", err)
			}
			engine, err := NewEngine(EngineConfig{HomeRoot: resolvedTempDir(t), Descriptor: testEngineDescriptor(1), Syncer: &credentialSyncer{client: client}})
			if err != nil {
				t.Fatal(err)
			}
			engine.backgroundSync(context.Background())
			if got := engine.status.State == "revoked"; got != test.want {
				t.Fatalf("revoked=%t for status=%d closeErr=%v, state=%q", got, test.status, test.closeErr, engine.status.State)
			}
		})
	}
}

func TestEngineConsumedFailuresAreDeduplicatedAndRecoverWithReference(t *testing.T) {
	reference := supportref.New()
	recorder := diagnostics.NewMemoryRecorder()
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), recorder)
	syncer := &retryingSyncer{failures: 2, err: syscall.ENOSPC}
	engine, err := NewEngine(EngineConfig{HomeRoot: resolvedTempDir(t), Descriptor: testEngineDescriptor(1), Syncer: syncer})
	if err != nil {
		t.Fatal(err)
	}
	var failures []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		if fault.Code == configSyncFailureCode {
			failures = append(failures, fault)
		}
	})
	defer restore()

	engine.backgroundSync(ctx)
	engine.backgroundSync(ctx)
	if len(failures) != 1 || failures[0].SupportReference != reference || failures[0].Errno != int(syscall.ENOSPC) {
		t.Fatalf("repeated failure observations = %#v", failures)
	}
	engine.backgroundSync(ctx)
	if engine.status.State != "healthy" {
		t.Fatalf("sync did not recover: %#v", engine.status)
	}
	recovered := 0
	for _, event := range recorder.Recent() {
		if event.Code == "recovered" {
			recovered++
			if event.SupportReference != reference {
				t.Fatalf("recovery reference = %q, want %q", event.SupportReference, reference)
			}
		}
	}
	if recovered != 1 {
		t.Fatalf("recovery events = %d, want one", recovered)
	}
}

func TestEngineRetryCancellationKeepsSubstantiveAttemptCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cause := syscall.ENOSPC
	engine, err := NewEngine(EngineConfig{
		HomeRoot: resolvedTempDir(t), Descriptor: testEngineDescriptor(2),
		Syncer: cancelingSyncer{cancel: cancel, cause: cause},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = engine.Apply(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("retry cancellation error lost original cause: %v", err)
	}
	fault := errorreport.ProjectFault(ctx, configSyncComponent, configSyncOperation, "reconciliation", configSyncFailureCode, err)
	if fault.Outcome != "failed" || fault.Errno != int(cause) || fault.Cause != "resource_exhausted" {
		t.Fatalf("mixed cancellation fault = %#v", fault)
	}
}

type failingCloseBody struct{ closeErr error }

func (failingCloseBody) Read([]byte) (int, error) { return 0, io.EOF }
func (body failingCloseBody) Close() error        { return body.closeErr }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
