package server

import (
	"encoding/json"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
)

func TestSuccessfulLaunchResultCarriesTheEnvironmentWarning(t *testing.T) {
	ctx, notices := envinject.WithLaunchNotices(t.Context())
	envinject.NoteLaunch(ctx, envinject.LaunchNoticePending)
	envinject.NoteLaunch(ctx, envinject.LaunchNoticePending)
	outcome := withLaunchWarnings(result(map[string]any{"snapshot": map[string]any{"id": "exec_1"}, "replay": false}), notices, false)
	var decoded struct {
		Snapshot map[string]any `json:"snapshot"`
		Warnings []string       `json:"launch_warnings"`
	}
	if err := json.Unmarshal(outcome.Result, &decoded); err != nil || decoded.Snapshot["id"] != "exec_1" || len(decoded.Warnings) != 1 || decoded.Warnings[0] != "environment_pending" {
		t.Fatalf("result=%s err=%v", outcome.Result, err)
	}
	if got := withLaunchWarnings(failure("invalid_request"), notices, false); got.ErrorCode != "invalid_request" || len(got.Result) != 0 {
		t.Fatalf("failure was annotated: %+v", got)
	}
	_, none := envinject.WithLaunchNotices(t.Context())
	if got := withLaunchWarnings(operation.Outcome{Result: json.RawMessage(`{"a":1}`)}, none, false); string(got.Result) != `{"a":1}` {
		t.Fatalf("clean launch changed: %s", got.Result)
	}
}

// F21-11: a launch admitted from the host's own rules while Paperboat was unreachable says ENV needs Paperboat online,
// not that delivery is pending; an undeliverable launch keeps its own code.
func TestOfflineLaunchSaysEnvironmentNeedsPaperboatOnline(t *testing.T) {
	ctx, notices := envinject.WithLaunchNotices(t.Context())
	envinject.NoteLaunch(ctx, envinject.LaunchNoticePending)
	envinject.NoteLaunch(ctx, envinject.LaunchNoticeUndeliverable)
	outcome := withLaunchWarnings(result(map[string]any{"snapshot": map[string]any{"id": "exec_1"}}), notices, true)
	var decoded struct {
		Warnings []string `json:"launch_warnings"`
	}
	if err := json.Unmarshal(outcome.Result, &decoded); err != nil || len(decoded.Warnings) != 2 || decoded.Warnings[0] != envinject.LaunchNoticeOffline || decoded.Warnings[1] != envinject.LaunchNoticeUndeliverable {
		t.Fatalf("result=%s err=%v", outcome.Result, err)
	}
}
