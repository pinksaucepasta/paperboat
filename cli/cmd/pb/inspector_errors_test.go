package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// F7-07: the daemon's inspector refusals reach the person as a coded message, not a generic failure.
func TestInspectorDaemonRefusalsAreCodedUserErrors(t *testing.T) {
	err := decodeInspectorDaemonError(http.StatusNotFound, []byte(`{"code":"inspector_not_enabled"}`))
	if !inspectorNotEnabled(err) {
		t.Fatalf("not-enabled refusal not recognised: %v", err)
	}
	result := classifyCLIJSONError(err)
	if result.Code != "inspector_not_enabled" || result.StateChanged != false || !strings.Contains(result.Message, "never captured") {
		t.Fatalf("code=%q state_changed=%v message=%q", result.Code, result.StateChanged, result.Message)
	}
	stale := classifyCLIJSONError(decodeInspectorDaemonError(http.StatusUnauthorized, []byte(`{"code":"inspector_authority_stale"}`)))
	if stale.Code != "inspector_authority_stale" || !strings.Contains(stale.Message, "list captures again") {
		t.Fatalf("code=%q message=%q", stale.Code, stale.Message)
	}
	ambiguous := classifyCLIJSONError(decodeInspectorDaemonError(http.StatusConflict, []byte(`{"code":"inspector_replay_ambiguous"}`)))
	if ambiguous.StateChanged != "unknown" || !ambiguous.OutcomeUncertain {
		t.Fatalf("ambiguous replay must stay uncertain: %+v", ambiguous)
	}
}

// X-43: `--body` without `--enable` is a usage error that names the fix, not "could not finish the command".
func TestInspectBodyWithoutEnableIsAUsageError(t *testing.T) {
	command := previewInspectCommand()
	if err := command.Flags().Set("body", "true"); err != nil {
		t.Fatal(err)
	}
	_, err := readTunnelInspectOptions(command)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "--enable") || strings.Contains(err.Error(), "could not finish") {
		t.Fatalf("err = %v", err)
	}
	if result := classifyCLIJSONError(err); !strings.Contains(result.Message, "--enable") {
		t.Fatalf("message = %q", result.Message)
	}
}
