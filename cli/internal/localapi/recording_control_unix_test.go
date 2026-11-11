//go:build darwin || linux

package localapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

type codedRefusal string

func (c codedRefusal) Error() string      { return string(c) }
func (c codedRefusal) RemoteCode() string { return string(c) }

// The daemon passes the host's recording refusals and its own codes to pb recordings start|stop, and
// keeps any other remote code private.
func TestRecordingControlCarriesRefusalCodes(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "recording.sock")
	answer := error(nil)
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }), RecordingControl: func(_ context.Context, _ Peer, request RecordingControlRequest) (RecordingControlResult, error) {
		if answer != nil {
			return RecordingControlResult{}, answer
		}
		return RecordingControlResult{SessionID: "s1", Recording: &wire.State{ID: "rec_00000000-0000-4000-8000-000000000001", Trigger: wire.TriggerRule, StartedAt: time.Unix(1, 0)}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := RecordingControlRequest{Action: wire.ActionStart}
	if result, err := client.RecordingControl(t.Context(), start); err != nil || result.SessionID != "s1" || result.Recording == nil {
		t.Fatalf("start %+v %v", result, err)
	}
	for refusal, code := range map[error]string{
		codedRefusal(wire.ErrorForbidden):                                         wire.ErrorForbidden,
		codedRefusal("some_host_detail"):                                          "recording_control_unavailable",
		&RecordingControlError{Code: RecordingAmbiguousCode, Message: "name one"}: RecordingAmbiguousCode,
	} {
		answer = refusal
		var remote *RemoteError
		if _, err := client.RecordingControl(t.Context(), start); !errors.As(err, &remote) || remote.Code != code {
			t.Fatalf("%v reached the CLI as %v", refusal, err)
		}
	}
	if _, err := client.RecordingControl(t.Context(), RecordingControlRequest{Action: "record-all"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an unknown action was sent: %v", err)
	}
	cancel()
	<-done
}
