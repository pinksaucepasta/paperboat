package updated

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestControlExchangeProjectsFiniteFailureCodeWithoutRemoteMessage(t *testing.T) {
	tests := []struct {
		name      string
		code      string
		operation string
		wantStage string
		wantCode  string
	}{
		{name: "known updater failure", code: "check_failed", operation: "download", wantStage: "update_download", wantCode: "updater_check_failed"},
		{name: "unknown updater code", code: "private/path", operation: "download", wantStage: "update_download", wantCode: "control_request_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				defer server.Close()
				var request ControlRequest
				if err := json.NewDecoder(server).Decode(&request); err != nil {
					t.Errorf("decode updater request: %v", err)
					return
				}
				if request.Operation != test.operation {
					t.Errorf("operation=%q, want %q", request.Operation, test.operation)
				}
				_ = json.NewEncoder(server).Encode(ControlResponse{
					Schema: ControlProtocolV1, Status: "error", ErrorCode: test.code,
					ErrorMessage: `open C:\Users\private\paperboat\state\update: Access is denied`,
				})
			}()

			_, err := exchangeControl(context.Background(), client, ControlRequest{Schema: ControlProtocolV1, Operation: test.operation}, time.Second, false)
			<-serverDone
			var controlErr *ControlError
			if !errors.As(err, &controlErr) {
				t.Fatalf("error=%T %v, want ControlError", err, err)
			}
			fault := errorreport.ProjectFault(context.Background(), "pb", "update", "command", "unexpected_cli_failure", err)
			if fault.Stage != test.wantStage || fault.Code != test.wantCode || fault.Cause != "internal" {
				t.Fatalf("projected stage=%q code=%q cause=%q", fault.Stage, fault.Code, fault.Cause)
			}
			encoded, marshalErr := json.Marshal(fault)
			if marshalErr != nil {
				t.Fatal("marshal projected fault")
			}
			if strings.Contains(string(encoded), "C:\\Users\\private") || strings.Contains(string(encoded), controlErr.Message) || strings.Contains(string(encoded), controlErr.Code) && test.wantCode == "control_request_failed" {
				t.Fatal("remote updater details escaped into diagnostic projection")
			}
		})
	}
}

func TestControlExchangeCancellationJoinsBlockedIOAndFreshRequestRecovers(t *testing.T) {
	client, server := net.Pipe()
	requestRead := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		var request ControlRequest
		if err := json.NewDecoder(server).Decode(&request); err != nil {
			t.Error("request decode failed")
			return
		}
		close(requestRead)
		_, _ = io.Copy(io.Discard, server)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exchangeControl(ctx, client, ControlRequest{Schema: ControlProtocolV1, Operation: "status"}, time.Minute, false)
		done <- err
	}()
	<-requestRead
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancellation lost: %T", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left blocked updater IO")
	}
	<-serverDone
	client, server = net.Pipe()
	go func() {
		defer server.Close()
		var request ControlRequest
		if err := json.NewDecoder(server).Decode(&request); err != nil {
			t.Error("recovery request decode failed")
			return
		}
		_ = json.NewEncoder(server).Encode(ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: "2026.10.08.1"})
	}()
	response, err := exchangeControl(context.Background(), client, ControlRequest{Schema: ControlProtocolV1, Operation: "status"}, time.Second, false)
	if err != nil || response.Version != "2026.10.08.1" {
		t.Fatal("fresh updater request did not recover")
	}
}

type brokenControlConnection struct {
	net.Conn
	failure error
}

func (c brokenControlConnection) Read([]byte) (int, error)    { return 0, c.failure }
func (c brokenControlConnection) Write(p []byte) (int, error) { return len(p), nil }
func (brokenControlConnection) SetDeadline(time.Time) error   { return nil }
func (brokenControlConnection) Close() error                  { return nil }

func TestControlExchangeKeepsOriginalMixedCauseWithoutFormatting(t *testing.T) {
	cause := errors.Join(context.Canceled, syscall.EIO)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exchangeControl(ctx, brokenControlConnection{failure: cause}, ControlRequest{Schema: ControlProtocolV1, Operation: "status"}, time.Second, false)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrInvalidControl) {
		t.Fatal("mixed updater cause was lost")
	}
	if controlCancellationOnly(err, context.Canceled) {
		t.Fatal("mixed failure became normal cancellation")
	}
	var failure controlClientFailure
	if !errors.As(err, &failure) || strings.Contains(failure.Error(), cause.Error()) {
		t.Fatal("updater error prose exposes original cause")
	}
}
