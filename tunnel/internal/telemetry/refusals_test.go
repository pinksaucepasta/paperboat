package telemetry

import (
	"fmt"
	"testing"
)

// Refusals add up per hostname until the heartbeat takes them; past the host bound only new names
// are dropped, names already counted keep counting.
func TestRefusalsCountPerHostAndStayBounded(t *testing.T) {
	TakeRefusals()
	NoteRefusal("a.example.test")
	NoteRefusal("a.example.test")
	NoteRefusal("")
	for i := 0; i < maxPendingRefusalHosts+10; i++ {
		NoteRefusal(fmt.Sprintf("h%d.example.test", i))
	}
	NoteRefusal("a.example.test")
	got := TakeRefusals()
	if len(got) != maxPendingRefusalHosts || got["a.example.test"] != 3 {
		t.Fatalf("%d hosts, a=%d", len(got), got["a.example.test"])
	}
	if again := TakeRefusals(); len(again) != 0 {
		t.Fatalf("counts not forgotten: %v", again)
	}
}
