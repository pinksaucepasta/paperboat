package tunnel

import (
	"context"
	"errors"
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
