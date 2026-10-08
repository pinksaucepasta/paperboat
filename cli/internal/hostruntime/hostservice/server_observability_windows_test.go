//go:build windows

package hostservice

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

func TestWindowsAuthorizedKeysFailureCapturesSafeCauseOnce(t *testing.T) {
	const sensitive = "sensitive authorized key source"
	server, err := New(Config{
		SocketPath:     `\\.\pipe\PaperboatHostServiceDiagnosticsTest`,
		StatePath:      filepath.Join(t.TempDir(), "availability.json"),
		Applier:        windowsTestApplier{},
		Version:        "test",
		AuthorizedKeys: &windowsTestAuthorizedKeys{err: errors.New(sensitive)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	reference := "support_123e4567-e89b-42d3-a456-426614174000"
	ctx := supportref.WithContext(context.Background(), reference)
	response := windowsPipeRequest(t, server, ctx, Request{Schema: ProtocolV1, Operation: "reconcile_ssh_authorized_keys", AuthorizedKeys: ptrTo([]string{})})
	if response.ErrorCode != "ssh_authorized_keys_reconcile_failed" {
		t.Fatalf("wire response exposed or changed its stable failure code: %+v", response)
	}
	if len(faults) != 1 {
		t.Fatalf("captured failures=%d", len(faults))
	}
	fault := faults[0]
	if fault.Operation != "ssh" || fault.Stage != "peer_authority" || fault.Code != "managed_ssh_failed" || fault.SupportReference != reference {
		t.Fatalf("fault lost SSH owner classification or reference: %+v", fault)
	}
	if strings.Contains(fault.Cause, sensitive) || strings.Contains(strings.Join(fault.ErrorChain, ","), sensitive) {
		t.Fatalf("fault retained arbitrary key-source text: %+v", fault)
	}
}

func TestWindowsAuthorizedKeysExpectedAndMixedFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		cause   error
		wantLog bool
	}{
		{name: "pure qualification rejection", cause: windowsopenssh.ErrQualificationEnrollment},
		{name: "mixed rejection and operational failure", cause: errors.Join(windowsopenssh.ErrQualificationEnrollment, errors.New("filesystem unavailable")), wantLog: true},
		{name: "pure cancellation", cause: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(Config{
				SocketPath:     `\\.\pipe\PaperboatHostServiceDiagnosticsTest`,
				StatePath:      filepath.Join(t.TempDir(), "availability.json"),
				Applier:        windowsTestApplier{},
				Version:        "test",
				AuthorizedKeys: &windowsTestAuthorizedKeys{err: test.cause},
			})
			if err != nil {
				t.Fatal(err)
			}
			var faults []errorreport.Fault
			restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
				faults = append(faults, fault)
			})
			defer restore()
			response := windowsPipeRequest(t, server, context.Background(), Request{Schema: ProtocolV1, Operation: "reconcile_ssh_authorized_keys", AuthorizedKeys: ptrTo([]string{})})
			if response.ErrorCode != "ssh_authorized_keys_reconcile_failed" {
				t.Fatalf("response=%+v", response)
			}
			if got := len(faults) == 1; got != test.wantLog {
				t.Fatalf("captured=%d, want fault=%t", len(faults), test.wantLog)
			}
		})
	}
}

func TestWindowsClientMapsOnlyFiniteResponseErrorCodes(t *testing.T) {
	for _, code := range []string{"invalid_request", "stale_policy", "availability_apply_failed", "update_activation_failed", "ssh_authorized_keys_reconcile_failed"} {
		if got := responseError(code).Error(); got != code {
			t.Fatalf("response code %q mapped to %q", code, got)
		}
	}
	const untrusted = "C:\\private\\credential.txt"
	if got := responseError(untrusted); got != ErrInvalidRequest || strings.Contains(got.Error(), untrusted) {
		t.Fatalf("unknown response code was echoed: %q", got)
	}
}

func TestWindowsRequestWriteFailureIsObservedWithoutExceptionCapture(t *testing.T) {
	server, err := New(Config{
		SocketPath: `\\.\pipe\PaperboatHostServiceDiagnosticsTest`,
		StatePath:  filepath.Join(t.TempDir(), "availability.json"),
		Applier:    windowsTestApplier{},
		Version:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	reference := "support_123e4567-e89b-42d3-a456-426614174000"
	ctx := supportref.WithContext(context.Background(), reference)
	serverSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(ctx, failedWriteConn{Conn: serverSide, err: errors.New("write failed")})
		close(done)
	}()
	if err := json.NewEncoder(clientSide).Encode(Request{Schema: ProtocolV1, Operation: "diagnostics"}); err != nil {
		t.Fatal(err)
	}
	_ = clientSide.Close()
	<-done
	if len(faults) != 1 {
		t.Fatalf("observed request write failure count=%d", len(faults))
	}
	fault := faults[0]
	if fault.Operation != "service" || fault.Stage != "lifecycle" || fault.Code != "service_failed" || fault.SupportReference != reference {
		t.Fatalf("write failure lost bounded owner metadata: %+v", fault)
	}
}

func windowsPipeRequest(t *testing.T, server *Server, ctx context.Context, request Request) Response {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.serveContext(ctx, serverSide)
		_ = serverSide.Close()
	}()
	if err := json.NewEncoder(clientSide).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.NewDecoder(clientSide).Decode(&response); err != nil {
		t.Fatal(err)
	}
	_ = clientSide.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return response
}

type failedWriteConn struct {
	net.Conn
	err error
}

func (c failedWriteConn) Write([]byte) (int, error) { return 0, c.err }

func ptrTo[T any](value T) *T { return &value }
