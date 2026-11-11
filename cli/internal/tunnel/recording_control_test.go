package tunnel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// Start and stop travel on the attachment and return the host's answer: the recording, or its refusal
// code for the CLI to explain.
func TestRecordingControlTravelsOnTheAttachment(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	connection, err := newInitializedHelperTerminalConn(context.Background(), message, &resolver.TerminalTarget{SessionID: "ses_rec", CWD: "/workspace"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = message.Close(); connection.finish(0, nil) }()
	link := RecordingController(connection)
	if link.RecordingSessionID() != "ses_rec" {
		t.Fatalf("session %q", link.RecordingSessionID())
	}
	state, err := link.RecordingControl(context.Background(), wire.ActionStart)
	if err != nil || state == nil || state.ID != "rec_00000000-0000-4000-8000-000000000001" || state.StartedBy != "acct_1" {
		t.Fatalf("start %+v %v", state, err)
	}
	var refused *RecordingRefusedError
	if _, err := link.RecordingControl(context.Background(), wire.ActionStop); !errors.As(err, &refused) || refused.RemoteCode() != wire.ErrorForbidden {
		t.Fatalf("stop refusal %v", err)
	}
	if _, err := link.RecordingControl(context.Background(), "record-everything"); err == nil {
		t.Fatal("an unknown action was sent")
	}
}

// pb connect carries the native terminal as a cursorTerminal; the daemon registers what it dialed, so the
// wrapper has to be the controller too (it was not: `pb recordings start` found no attachment on a device).
func TestCursorTerminalControlsItsSessionsRecording(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	helper, err := newInitializedHelperTerminalConn(context.Background(), message, &resolver.TerminalTarget{SessionID: "ses_rec", CWD: "/workspace"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	terminal := newCursorTerminal(context.Background(), helper, &atomic.Int64{}, func(int) {})
	defer func() { _ = terminal.Close(); _ = message.Close(); helper.finish(0, nil) }()
	var link Conn = terminal // what Dial returns
	controller, ok := link.(RecordingController)
	if !ok {
		t.Fatal("the dialed terminal is not a RecordingController")
	}
	if controller.RecordingSessionID() != "ses_rec" {
		t.Fatalf("session %q", controller.RecordingSessionID())
	}
	state, err := controller.RecordingControl(context.Background(), wire.ActionStart)
	if err != nil || state == nil || state.StartedBy != "acct_1" {
		t.Fatalf("start %+v %v", state, err)
	}
}

// The REC state of a link reaches an observer synchronously, so the local API can send it before the first output
// (CHECKLIST F25-02). Through the reconnect supervisor it arrived after the first output on a device.
func TestCursorTerminalTellsARecordingObserverAtOnce(t *testing.T) {
	message := newEarlyOutputMessageConnection()
	helper, err := newInitializedHelperTerminalConn(context.Background(), message, &resolver.TerminalTarget{SessionID: "ses_rec", CWD: "/workspace"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	terminal := newCursorTerminal(context.Background(), helper, &atomic.Int64{}, func(int) {})
	defer func() { _ = terminal.Close(); _ = message.Close(); helper.finish(0, nil) }()
	helper.recordingState.report(true)
	var got []bool
	terminal.ObserveRecording(func(recording bool) { got = append(got, recording) })
	if len(got) != 1 || !got[0] {
		t.Fatalf("observer told %v at registration, want [true]", got)
	}
	helper.recordingState.report(false)
	if len(got) != 2 || got[1] {
		t.Fatalf("a change was not delivered in order: %v", got)
	}
}
