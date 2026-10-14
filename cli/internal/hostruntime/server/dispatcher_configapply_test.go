package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configapply"
)

func TestDispatcherConfigApplyConformance(t *testing.T) {
	dispatcher := &Dispatcher{config: DispatcherConfig{ConfigApply: configapply.ConformanceHandler{}}}
	capabilities := dispatcher.Capabilities()
	if !contains(capabilities, "config.apply.v1") {
		t.Fatalf("capabilities = %v", capabilities)
	}

	authorization := Authorization{ResourceID: "asg_test_01"}
	stale := dispatcher.Handle(context.Background(), authorization, "config.apply.v1", json.RawMessage(`{"action":"apply","assignment_id":"asg_test_01","expected_revision":"rev_1","observed_revision":"rev_2"}`))
	if stale.ErrorCode != "config_revision_conflict" {
		t.Fatalf("error = %q", stale.ErrorCode)
	}

	accepted := dispatcher.Handle(context.Background(), authorization, "config.apply.v1", json.RawMessage(`{"action":"apply","assignment_id":"asg_test_01","expected_revision":"rev_2","observed_revision":"rev_2"}`))
	if accepted.ErrorCode != "" || !json.Valid(accepted.Result) {
		t.Fatalf("outcome = %+v", accepted)
	}
	var result configapply.Result
	if err := json.Unmarshal(accepted.Result, &result); err != nil || result.Applied {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestDispatcherConfigApplyHidesOtherAssignments(t *testing.T) {
	dispatcher := &Dispatcher{config: DispatcherConfig{ConfigApply: configapply.ConformanceHandler{}}}
	outcome := dispatcher.Handle(context.Background(), Authorization{ResourceID: "asg_other"}, "config.apply.v1", json.RawMessage(`{"action":"apply","assignment_id":"asg_test_01","expected_revision":"rev_2","observed_revision":"rev_2"}`))
	if outcome.ErrorCode != "not_found_or_forbidden" {
		t.Fatalf("error = %q", outcome.ErrorCode)
	}
}

func TestDispatcherConfigApplyKeepsOnlyPureDomainErrorsActionable(t *testing.T) {
	operational := errors.New("private storage failure")
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "operational", err: operational, want: "unavailable"},
		{name: "invalid request with operational failure", err: errors.Join(configapply.ErrInvalidRequest, operational), want: "unavailable"},
		{name: "revision conflict with operational failure", err: errors.Join(configapply.ErrRevisionConflict, operational), want: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher := &Dispatcher{config: DispatcherConfig{ConfigApply: configapply.SyncHandler{
				Apply: func(context.Context) error { return test.err },
			}}}
			outcome := dispatcher.Handle(context.Background(), Authorization{ResourceID: "asg_test_01"}, "config.apply.v1", json.RawMessage(`{"action":"apply","assignment_id":"asg_test_01","expected_revision":"rev_1","observed_revision":"rev_1"}`))
			if outcome.ErrorCode != test.want || len(outcome.Result) != 0 {
				t.Fatalf("outcome=%+v want error code %q without result", outcome, test.want)
			}
		})
	}

	dispatcher := &Dispatcher{config: DispatcherConfig{ConfigApply: configapply.ConformanceHandler{}}}
	invalid := dispatcher.Handle(context.Background(), Authorization{ResourceID: "asg_test_01"}, "config.apply.v1", json.RawMessage(`{"action":"unknown","assignment_id":"asg_test_01","expected_revision":"rev_1","observed_revision":"rev_1"}`))
	if invalid.ErrorCode != "invalid_request" {
		t.Fatalf("pure invalid request code=%q", invalid.ErrorCode)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
