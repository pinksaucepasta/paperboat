package localdaemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

type fakeLink struct {
	session string
	calls   int
}

func (f *fakeLink) RecordingSessionID() string { return f.session }
func (f *fakeLink) RecordingControl(_ context.Context, action string) (*wire.State, error) {
	f.calls++
	if action == wire.ActionStop {
		return nil, nil
	}
	return &wire.State{ID: "rec_1", Trigger: wire.TriggerManual, StartedBy: "acct_1", StartedAt: time.Unix(1, 0)}, nil
}

// pb recordings start acts on the one attached session, on its newest link, and asks which one when
// several are attached.
func TestRecordingControlPicksTheAttachedSession(t *testing.T) {
	links := &terminalLinks{}
	start := localapi.RecordingControlRequest{Action: wire.ActionStart}
	var refused *localapi.RecordingControlError
	if _, err := links.control(context.Background(), start); !errors.As(err, &refused) || refused.Code != localapi.RecordingNotAttachedCode {
		t.Fatalf("nothing attached: %v", err)
	}
	old, newest := &fakeLink{session: "s1"}, &fakeLink{session: "s1"}
	links.add(old)
	removeNewest := links.add(newest)
	result, err := links.control(context.Background(), start)
	if err != nil || result.SessionID != "s1" || result.Recording == nil || newest.calls != 1 || old.calls != 0 {
		t.Fatalf("start %+v %v (old %d newest %d)", result, err, old.calls, newest.calls)
	}
	removeOther := links.add(&fakeLink{session: "s2"})
	if _, err := links.control(context.Background(), start); !errors.As(err, &refused) || refused.Code != localapi.RecordingAmbiguousCode {
		t.Fatalf("two sessions: %v", err)
	}
	result, err = links.control(context.Background(), localapi.RecordingControlRequest{SessionID: "s1", Action: wire.ActionStop})
	if err != nil || result.Recording != nil || newest.calls != 2 {
		t.Fatalf("named stop %+v %v", result, err)
	}
	removeNewest()
	removeOther()
	if _, err := links.control(context.Background(), start); err != nil || old.calls != 1 {
		t.Fatalf("after the newer link ended: %v", err)
	}
}
