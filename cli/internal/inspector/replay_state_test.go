package inspector

import "testing"

// X-43: a replayed binary response is reported complete (it is stored as sent), not unsupported.
func TestReplayResponseStateIsCompleteForBinary(t *testing.T) {
	if got := responseCaptureState(false); got != StateComplete {
		t.Fatalf("state = %q", got)
	}
	if got := responseCaptureState(true); got != StateTruncated {
		t.Fatalf("truncated state = %q", got)
	}
}
