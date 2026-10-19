//go:build darwin || linux

package updated

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestHTTPHealthFiniteFailureReasonsPreserveChecks(t *testing.T) {
	fresh := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "candidate", Epoch: 2, LastHeartbeatUnixMilli: time.Now().UnixMilli()}
	for _, tc := range []struct {
		name, reason, body string
		code               int
		alter              func(*hostdproto.Status)
	}{
		{name: "fresh", code: 200, body: `{"live":true}`},
		{name: "wrong owner", reason: "owner_identity", alter: func(s *hostdproto.Status) { s.Epoch = 0 }},
		{name: "missing heartbeat", reason: "heartbeat_missing", alter: func(s *hostdproto.Status) { s.LastHeartbeatUnixMilli = 0 }},
		{name: "stale heartbeat", reason: "heartbeat_stale", alter: func(s *hostdproto.Status) { s.LastHeartbeatUnixMilli = time.Now().Add(-16 * time.Second).UnixMilli() }},
		{name: "redirect", reason: "http_redirect", code: 302, body: "private payload"},
		{name: "status", reason: "http_status", code: 503, body: "private payload"},
		{name: "malformed", reason: "http_body", code: 200, body: "private payload"},
		{name: "not live", reason: "http_not_live", code: 200, body: `{"live":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("unexpected health path")
				}
				if tc.code == 302 {
					w.Header().Set("Location", "/private")
				}
				code := tc.code
				if code == 0 {
					code = 200
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			status := fresh
			if tc.alter != nil {
				tc.alter(&status)
			}
			err := (HTTPHealth{Endpoint: server.URL + "/healthz"}).Check(context.Background(), status, workerupdate.Release{})
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			assertActivationFailure(t, err, "health", tc.reason, ErrInvalidConfig)
		})
	}
	err := (HTTPHealth{Endpoint: "https://secret.invalid/healthz?token=private"}).Check(context.Background(), fresh, workerupdate.Release{})
	assertActivationFailure(t, err, "health", "endpoint_invalid", ErrInvalidConfig)
}

type failureTransport struct{ err error }

func (r failureTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

type healthResponseTransport struct{ response *http.Response }

func (r healthResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return r.response, nil
}

type healthResponseBody struct {
	reader   io.Reader
	readErr  error
	closeErr error
}

func (b healthResponseBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.reader.Read(p)
}

func (b healthResponseBody) Close() error { return b.closeErr }

func TestHTTPHealthTransportRetainsCancellationWithoutFormattingCause(t *testing.T) {
	status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "candidate", Epoch: 2, LastHeartbeatUnixMilli: time.Now().UnixMilli()}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("private network endpoint and credential")} {
		err := (HTTPHealth{Endpoint: "http://127.0.0.1:1/healthz", Client: &http.Client{Transport: failureTransport{cause}}}).Check(context.Background(), status, workerupdate.Release{})
		assertActivationFailure(t, err, "health", "transport", cause)
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("wrapped cause exposed: %s", err)
		}
	}
}

func TestHTTPHealthRejectsUnreadableOrUnclosedBody(t *testing.T) {
	status := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "candidate", Epoch: 2, LastHeartbeatUnixMilli: time.Now().UnixMilli()}
	readErr := errors.New("private read failure")
	closeErr := errors.New("private close failure")
	for _, tc := range []struct {
		name  string
		body  healthResponseBody
		cause error
	}{
		{name: "read", body: healthResponseBody{readErr: readErr}, cause: readErr},
		{name: "close", body: healthResponseBody{reader: bytes.NewReader([]byte(`{"live":true}`)), closeErr: closeErr}, cause: closeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusOK, Body: tc.body}
			health := HTTPHealth{Endpoint: "http://127.0.0.1/healthz", Client: &http.Client{Transport: healthResponseTransport{response}}}
			err := health.Check(context.Background(), status, workerupdate.Release{})
			if err == nil || !strings.Contains(err.Error(), "health") || !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, tc.cause) {
				t.Fatalf("health error=%v; expected static failure with original body cause", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("body error prose leaked: %v", err)
			}
		})
	}
}

func TestUnixParticipantFiniteFailureReasonsPreserveReadiness(t *testing.T) {
	baseline := UnixParticipantProbe{Running: true, State: "ready", Version: "2026.10.08.20", UpdaterVersion: "2026.10.08.20"}
	version := "2026.10.08.31"
	fresh := UnixParticipantProbe{Running: true, State: "ready", Version: version, UpdaterVersion: version}
	for _, tc := range []struct {
		reason string
		alter  func(*UnixParticipantProbe)
	}{
		{"updater_version", func(p *UnixParticipantProbe) { p.UpdaterVersion = "private version" }},
		{"daemon_stopped", func(p *UnixParticipantProbe) { p.Running = false }},
		{"daemon_version", func(p *UnixParticipantProbe) { p.Version = "private version" }},
		{"daemon_state", func(p *UnixParticipantProbe) { p.State = "private state" }},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			probe := fresh
			tc.alter(&probe)
			assertActivationFailure(t, unixParticipantReady(probe, version, baseline, true), "participant", tc.reason, ErrParticipantReadiness)
		})
	}
	if err := unixParticipantReady(fresh, version, baseline, true); err != nil {
		t.Fatal(err)
	}
	degraded := fresh
	degraded.State, degraded.ControlPlaneUnavailableOnly = "degraded", true
	if err := unixParticipantReady(degraded, version, degraded, false); err != nil {
		t.Fatalf("lost existing degraded recovery exception: %v", err)
	}
	assertActivationFailure(t, unixParticipantReady(degraded, version, degraded, true), "participant", "daemon_state", ErrParticipantReadiness)
}

func assertActivationFailure(t *testing.T, err error, phase, reason string, cause error) {
	t.Helper()
	var failure *activationFailure
	if !errors.As(err, &failure) || failure.phase != phase || failure.reason != reason || !errors.Is(err, cause) {
		t.Fatalf("failure identity=%v expected=%s/%s cause retained=%v", err, phase, reason, errors.Is(err, cause))
	}
}

func TestActivationFailureStderrIsBoundedAndDoesNotFormatPrivateErrors(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		err                           error
		previous, candidate, expected string
	}{
		{"joined timeout", errors.Join(participantFailure("updater_version", ErrParticipantReadiness), context.DeadlineExceeded, errors.New("private secret")), "2026.10.08.20", "2026.10.08.31", "paperboat-update-failure phase=participant reason=updater_version previous=2026.10.08.20 candidate=2026.10.08.31 classification=timeout chain=*errors.joinError,*updated.activationFailure,context.deadlineExceededError,*errors.errorString io=none sentinels=ParticipantReadiness\n"},
		{"transport cancellation", healthFailure("transport", context.Canceled), "2026.10.08.20", "2026.10.08.31", "paperboat-update-failure phase=health reason=transport previous=2026.10.08.20 candidate=2026.10.08.31 classification=canceled chain=*updated.activationFailure,*errors.errorString io=none sentinels=none\n"},
		{"transport timeout", healthFailure("transport", os.ErrDeadlineExceeded), "2026.10.08.20", "2026.10.08.31", "paperboat-update-failure phase=health reason=transport previous=2026.10.08.20 candidate=2026.10.08.31 classification=timeout chain=*updated.activationFailure,*poll.DeadlineExceededError io=none sentinels=none\n"},
		{"unsafe fields", &activationFailure{phase: "private\nphase", reason: "private\nreason", cause: errors.New("private secret")}, "private\nversion", strings.Repeat("1", 100) + ".1.1.1", "paperboat-update-failure phase=activation reason=failed previous=unknown candidate=unknown classification=failure chain=*updated.activationFailure,*errors.errorString io=none sentinels=none\n"},
		{"untyped failure", errors.New("private secret"), "2026.10.08.20", "2026.10.08.31", "paperboat-update-failure phase=activation reason=failed previous=2026.10.08.20 candidate=2026.10.08.31 classification=failure chain=*errors.errorString io=none sentinels=none\n"},
		{"success", nil, "private", "private", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			writeActivationFailure(&output, tc.err, tc.previous, tc.candidate)
			if output.String() != tc.expected {
				t.Fatalf("bounded diagnostic mismatch: %q", output.String())
			}
		})
	}
}

func TestActivationFailureChainNamesTypesNotMessages(t *testing.T) {
	secret := errors.New("/home/private/credential-token-123")
	err := errors.Join(fmt.Errorf("step failed: %w", &exec.ExitError{}), secret)
	chain, cause, _ := activationErrorChain(err)
	if cause != "none" {
		t.Fatalf("no file operation was involved; io=%q", cause)
	}
	if !strings.Contains(chain, "*exec.ExitError") || strings.Contains(chain, "credential") || strings.Contains(chain, "private") {
		t.Fatalf("chain=%q", chain)
	}
	if got, cause, named := activationErrorChain(nil); got != "none" || cause != "none" || named != "none" {
		t.Fatalf("nil chain=%q io=%q sentinels=%q", got, cause, named)
	}
	loop := &selfCause{}
	loop.next = loop
	if got, _, _ := activationErrorChain(loop); got == "" || len(strings.Split(got, ",")) > 8 {
		t.Fatalf("cyclic chain was not bounded: %q", got)
	}
}

func TestActivationFailureNamesTheFileOperationNotThePath(t *testing.T) {
	err := errors.Join(workerupdate.ErrBlocked, &fs.PathError{Op: "lstat", Path: "/home/private/secret-token", Err: syscall.ENOENT})
	_, cause, named := activationErrorChain(err)
	if cause != "lstat:ENOENT" || named != "Blocked" {
		t.Fatalf("io=%q sentinels=%q", cause, named)
	}
	var output strings.Builder
	writeActivationFailure(&output, err, "2026.10.19.8", "2026.10.19.9")
	if !strings.Contains(output.String(), " io=lstat:ENOENT sentinels=Blocked\n") || strings.Contains(output.String(), "secret") {
		t.Fatalf("record=%q", output.String())
	}
}

type selfCause struct{ next *selfCause }

func (e *selfCause) Error() string { return "cycle" }
func (e *selfCause) Unwrap() error { return e.next }
