package telemetry

import "testing"

func TestErrorPagesAreTakenOnceAndBounded(t *testing.T) {
	TakeErrorPages()
	for i := 0; i < maxPendingErrorPages+10; i++ {
		NoteErrorPage("support_00000000-0000-4000-8000-000000000000", 502)
	}
	if got := TakeErrorPages(); len(got) != maxPendingErrorPages || got[0].Status != 502 {
		t.Fatalf("took %d pages, want the %d bound", len(got), maxPendingErrorPages)
	}
	if got := TakeErrorPages(); len(got) != 0 {
		t.Fatalf("second take returned %d pages", len(got))
	}
}
