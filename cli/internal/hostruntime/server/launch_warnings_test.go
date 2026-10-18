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
	outcome := withLaunchWarnings(result(map[string]any{"snapshot": map[string]any{"id": "exec_1"}, "replay": false}), notices)
	var decoded struct {
		Snapshot map[string]any `json:"snapshot"`
		Warnings []string       `json:"launch_warnings"`
	}
	if err := json.Unmarshal(outcome.Result, &decoded); err != nil || decoded.Snapshot["id"] != "exec_1" || len(decoded.Warnings) != 1 || decoded.Warnings[0] != "environment_pending" {
		t.Fatalf("result=%s err=%v", outcome.Result, err)
	}
	if got := withLaunchWarnings(failure("invalid_request"), notices); got.ErrorCode != "invalid_request" || len(got.Result) != 0 {
		t.Fatalf("failure was annotated: %+v", got)
	}
	_, none := envinject.WithLaunchNotices(t.Context())
	if got := withLaunchWarnings(operation.Outcome{Result: json.RawMessage(`{"a":1}`)}, none); string(got.Result) != `{"a":1}` {
		t.Fatalf("clean launch changed: %s", got.Result)
	}
}
