package localdaemon

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestReproductionMarkersRetainInvocationReference(t *testing.T) {
	recorder := diagnostics.NewMemoryRecorder()
	service := diagnosticService{recorder: recorder}
	ctx := supportref.WithContext(t.Context(), supportref.New())
	for _, phase := range []string{"before", "after", "unexpected_cli_failure"} {
		if err := service.RecordBugreportMarker(ctx, phase); err != nil {
			t.Fatal(err)
		}
	}
	records := recorder.Recent()
	if len(records) != 3 {
		t.Fatal("reproduction markers missing")
	}
	for _, record := range records {
		if record.SupportReference != supportref.FromContext(ctx) {
			t.Fatal("reproduction marker has unrelated reference")
		}
	}
}
