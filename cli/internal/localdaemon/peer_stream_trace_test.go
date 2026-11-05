package localdaemon

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// An ssh or exec dial records its steps under the request's support reference, like a terminal redial; a terminal
// keeps its own trace.
func TestNonTerminalDialsRecordTheirSteps(t *testing.T) {
	recorder := diagnostics.NewMemoryRecorder()
	ref := supportref.New()
	request := supportref.WithContext(diagnostics.WithRecorder(t.Context(), recorder), ref)
	for _, consumer := range []string{"ssh", "exec", "config_compare"} {
		diagnostics.ReconnectStep(tracedDial(request, t.Context(), consumer), "regional_prepare_"+consumer)(nil)
	}
	diagnostics.ReconnectStep(tracedDial(request, t.Context(), "terminal"), "regional_prepare_terminal")(nil)
	steps := map[string]bool{}
	for _, e := range recorder.Recent() {
		if e.Code == "redial_step" && e.SupportReference == ref && e.Fields["attempt_id"] != "" && e.Fields["caller"] == "target" {
			steps[e.Fields["step"]+"/"+e.Fields["state"]] = true
		}
	}
	for _, consumer := range []string{"ssh", "exec", "config_compare"} {
		step := "regional_prepare_" + consumer
		if !steps[step+"/started"] || !steps[step+"/ready"] {
			t.Fatal("missing step records for", consumer, steps)
		}
	}
	if steps["regional_prepare_terminal/started"] {
		t.Fatal("a terminal dial was traced by the stream opener")
	}
}
