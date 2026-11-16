package diagnostics

import (
	"testing"
	"time"
)

// X-41: the same failure from the same call site is recorded once per window, whatever its support reference.
func TestRepeatedFailureFromOneCallSiteIsRecordedOncePerWindow(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	recorder := NewMemoryRecorder()
	recorder.clock = func() time.Time { return now }
	fields := func(site string) map[string]string {
		return map[string]string{"component": "paperboat-daemon", "operation": "engine_tunnels", "cause": "permission_denied", "source_function": site}
	}
	for range 10 {
		if err := recorder.RecordWithSupportReference("control_request", "control_request_failed", "warning", "support_"+now.String(), fields("a.refresh")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(recorder.Recent()); got != 1 {
		t.Fatalf("records after ten repeats = %d", got)
	}
	_ = recorder.Record("control_request", "control_request_failed", "warning", fields("b.refresh"))
	if got := len(recorder.Recent()); got != 2 {
		t.Fatalf("another call site's failure was suppressed: %d records", got)
	}
	now = now.Add(repeatWindow)
	_ = recorder.Record("control_request", "control_request_failed", "warning", fields("a.refresh"))
	if got := len(recorder.Recent()); got != 3 {
		t.Fatalf("the failure was not recorded again after the window: %d records", got)
	}
	// a record that names no call site (a one-off event) is never held back
	for range 3 {
		_ = recorder.Record("lifecycle", "component_started", "info", map[string]string{"component": "x"})
	}
	if got := len(recorder.Recent()); got != 6 {
		t.Fatalf("records without a call site were suppressed: %d records", got)
	}
}
