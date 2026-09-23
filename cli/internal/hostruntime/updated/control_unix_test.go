//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestControlStatusRejectsUnreadableActivationState(t *testing.T) {
	root := t.TempDir()
	// An activation directory replaced by a regular file must never be
	// interpreted as a completed update, regardless of the caller's UID.
	if err := os.WriteFile(filepath.Join(root, "activation"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Service{manager: &workerupdate.Manager{}, config: Config{StateRoot: root}}
	_, err := s.controlRequestWithRequest(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: "status"})
	if !errors.Is(err, ErrRecoveryState) {
		t.Fatal("status accepted unreadable activation state as completed")
	}
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := controlServer{uid: os.Geteuid(), gid: os.Getegid(), invokeRequest: s.controlRequestWithRequest}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.AcceptUnix()
		if err == nil {
			defer connection.Close()
			_ = server.handle(connection)
		}
	}()
	client, err := NewClient(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Status(context.Background())
	var controlErr *ControlError
	if !errors.As(err, &controlErr) || controlErr.Code != "recovery_required" || controlErr.Message != ErrRecoveryState.Error() {
		t.Fatalf("client did not receive actionable recovery error: %v", err)
	}
	<-done
}

func TestControlClientMarksMissingUpdaterAsUnavailable(t *testing.T) {
	client, err := NewClient("/tmp/paperboat-updated-does-not-exist.sock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Check(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing updater error = %v, want ErrUnavailable", err)
	}
}

func TestControlClientUsesOnlyFixedOperations(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := controlServer{socketPath: path, uid: os.Geteuid(), gid: os.Getegid(), invoke: func(_ context.Context, operation string) (ControlResponse, error) {
		if operation != "update" {
			t.Fatalf("operation = %q", operation)
		}
		return ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: "2.0.0", Updated: true}, nil
	}}
	done := make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_ = server.handle(connection)
			_ = connection.Close()
		}
		close(done)
	}()
	client, err := NewClient(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Version != "2.0.0" || !response.Updated {
		t.Fatalf("response = %#v", response)
	}
	<-done
}

func TestControlClientSurfacesBoundedUpdaterDiagnostic(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := controlServer{socketPath: path, uid: os.Geteuid(), gid: os.Getegid(), invoke: func(context.Context, string) (ControlResponse, error) {
		return ControlResponse{}, errors.New("service target is invalid\nrestart required")
	}}
	done := make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_ = server.handle(connection)
			_ = connection.Close()
		}
		close(done)
	}()
	client, err := NewClient(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Check(context.Background())
	var controlErr *ControlError
	if !errors.As(err, &controlErr) || controlErr.Code != "update_failed" || controlErr.Message != "service target is invalid restart required" {
		t.Fatalf("error=%#v", err)
	}
	<-done
}

func TestControlRejectsUnknownFields(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := controlServer{socketPath: path, uid: os.Geteuid(), gid: os.Getegid(), invoke: func(context.Context, string) (ControlResponse, error) {
		t.Fatal("invoke must not run")
		return ControlResponse{}, nil
	}}
	done := make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_ = server.handle(connection)
			_ = connection.Close()
		}
		close(done)
	}()
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte(`{"schema":"paperboat.updated/v1","operation":"status","unexpected":true}\n`)); err != nil {
		t.Fatal(err)
	}
	if closer, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
	_ = connection.Close()
	<-done
}

func TestControlErrorCodeReportsActiveTerminalAdmission(t *testing.T) {
	err := errors.Join(ErrParticipantReadiness, &autoupdate.ActiveTerminalSessionsError{RequiredVersion: "2026.08.27.47"})
	if code := controlErrorCode(err); code != autoupdate.BlockedActiveTerminalSessions {
		t.Fatalf("code=%q", code)
	}
}

func TestControlStatusRemainsResponsiveDuringUpdate(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	updateStarted := make(chan struct{})
	releaseUpdate := make(chan struct{})
	server := controlServer{socketPath: path, uid: os.Geteuid(), gid: os.Getegid(), invokeRequest: func(_ context.Context, request ControlRequest) (ControlResponse, error) {
		switch request.Operation {
		case "update":
			close(updateStarted)
			<-releaseUpdate
			return ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: "2.0.0", Updated: true}, nil
		case "status":
			return ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: "1.0.0"}, nil
		default:
			return ControlResponse{}, ErrInvalidControl
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.serve(ctx, listener)
	client, err := NewClient(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	updateDone := make(chan error, 1)
	go func() {
		_, updateErr := client.Update(context.Background())
		updateDone <- updateErr
	}()
	select {
	case <-updateStarted:
	case <-time.After(time.Second):
		t.Fatal("update did not reach the control server")
	}
	status, err := client.Status(context.Background())
	if err != nil || status.Version != "1.0.0" {
		t.Fatalf("status during update = %#v, %v", status, err)
	}
	close(releaseUpdate)
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
}

func TestControlResponseIsReadableBeforePostCommitHandoffCompletes(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	handoffStarted := make(chan struct{})
	releaseHandoff := make(chan struct{})
	server := controlServer{
		socketPath: path,
		uid:        os.Geteuid(),
		gid:        os.Getegid(),
		invokeRequest: func(context.Context, ControlRequest) (ControlResponse, error) {
			return ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: "2.0.0", Updated: true}, nil
		},
		afterResponse: func(ControlRequest, ControlResponse) {
			close(handoffStarted)
			<-releaseHandoff
		},
	}
	done := make(chan struct{})
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_ = server.handle(connection)
			_ = connection.Close()
		}
		close(done)
	}()
	client, err := NewClient(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Update(context.Background())
	if err != nil || response.Version != "2.0.0" || !response.Updated {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	select {
	case <-handoffStarted:
	case <-time.After(time.Second):
		t.Fatal("post-response handoff did not start")
	}
	close(releaseHandoff)
	<-done
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	file, err := os.CreateTemp("", "pbupd-")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return path
}
