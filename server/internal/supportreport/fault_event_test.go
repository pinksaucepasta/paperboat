package supportreport

import (
	"encoding/json"
	"strings"
	"syscall"
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestFaultEventBuilderUsesProjectedCauseAndBoundedSourceOnly(t *testing.T) {
	original := &privateDiagnosticError{cause: syscall.EIO}
	ctx := WithFailureIdentity(t.Context(), "", "", NewReference())
	fault := ProjectFault(ctx, "paperboat-server", "backup_operation", "backup_execution", "service_failed", original)
	stack := &sentry.Stacktrace{Frames: []sentry.Frame{{Function: "private/SECRET/producer.Backup", Module: "private/SECRET/producer", Filename: "/private/SECRET/producer.go", AbsPath: "SECRET", Vars: map[string]any{"token": "SECRET"}, ContextLine: "SECRET", Lineno: 27}}}
	event := NewFaultEvent(fault, stack)
	if event == nil || len(event.Exception) != 1 || event.Exception[0].Value != "system_error" || event.Tags["errno"] == "" || event.Tags["support_reference"] != fault.SupportReference {
		t.Fatal("shared fault event lost original cause or identity")
	}
	encoded, err := json.Marshal(event)
	if err != nil || strings.Contains(string(encoded), "SECRET") || original.calls != 0 {
		t.Fatal("shared fault builder exposed private error or stack data")
	}
	event.Message = "SECRET"
	event.Contexts["untrusted"] = sentry.Context{"token": "SECRET"}
	event.Tags["untrusted"] = "SECRET"
	clean := SanitizeFaultEvent(event)
	encoded, err = json.Marshal(clean)
	if clean == nil || err != nil || strings.Contains(string(encoded), "SECRET") {
		t.Fatal("SDK event augmentation bypassed shared sanitizer")
	}
	stack.Frames[0].Module = strings.Repeat("x", 1000)
	event = NewFaultEvent(fault, stack)
	if len(event.Exception[0].Stacktrace.Frames[0].Module) > 64 {
		t.Fatal("source module escaped stack bounds")
	}
	if SanitizeFaultEvent(nil) != nil || SanitizeFaultEvent(&sentry.Event{Message: "SECRET"}) != nil {
		t.Fatal("ordinary SDK event was admitted as an owned fault")
	}
}
