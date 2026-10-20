//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type pendingEnvironment struct{ err error }

func (p pendingEnvironment) Environment() ([]string, error) { return nil, p.err }

type recordingExecutions struct {
	execprocess.Service
	launch []string
	called bool
}

func (r *recordingExecutions) Start(ctx context.Context, _ execprocess.Request) (execprocess.ExecutionService, bool, error) {
	r.called, r.launch = true, execprocess.LaunchEnvironment(ctx)
	return nil, false, nil
}

type recordingSessions struct {
	session.Service
	launch []string
	called bool
}

func (r *recordingSessions) Create(ctx context.Context, _ session.CreateRequest) (session.Snapshot, error) {
	r.called, r.launch = true, session.LaunchEnvironment(ctx)
	return session.Snapshot{}, nil
}

// Product rule: managed ENV is an addition to a launch, never a precondition.
// Pending or undeliverable values start the exec or terminal without them and
// leave one notice for the caller to show.
func TestLaunchesRunWithoutUndeliverableManagedEnvironmentAndNoteIt(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		want  string
	}{
		"not ready": {envinject.ErrNotReady, envinject.LaunchNoticePending},
		"delivery":  {&envinject.DeliveryError{Cause: errors.New("private ENV failure")}, envinject.LaunchNoticeUndeliverable},
		"joined":    {errors.Join(errors.New("transport"), &envinject.DeliveryError{Cause: envinject.ErrNotReady}), envinject.LaunchNoticePending},
	} {
		cause := tc.cause
		t.Run(name, func(t *testing.T) {
			source := pendingEnvironment{err: cause}
			ctx, notices := envinject.WithLaunchNotices(t.Context())
			executions := &recordingExecutions{}
			if _, _, err := (launchExecutions{Service: executions, source: source}).Start(ctx, execprocess.Request{}); err != nil || !executions.called || len(executions.launch) != 0 {
				t.Fatalf("exec start err=%v called=%v env=%v", err, executions.called, executions.launch)
			}
			sessions := &recordingSessions{}
			if _, err := (launchSessions{Service: sessions, source: source}).Create(ctx, session.CreateRequest{}); err != nil || !sessions.called || len(sessions.launch) != 0 {
				t.Fatalf("terminal create err=%v called=%v env=%v", err, sessions.called, sessions.launch)
			}
			if codes := notices.Codes(); len(codes) != 1 || codes[0] != tc.want {
				t.Fatalf("notices=%v, want exactly %s", codes, tc.want)
			}
		})
	}
}

func TestLaunchStillStopsForCancellationNotForMissingEnvironment(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	executions := &recordingExecutions{}
	if _, _, err := (launchExecutions{Service: executions, source: pendingEnvironment{err: envinject.ErrNotReady}}).Start(ctx, execprocess.Request{}); err == nil || executions.called {
		t.Fatalf("canceled launch started: err=%v", err)
	}
}
