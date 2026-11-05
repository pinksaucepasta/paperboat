package diagnostics

import (
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func TestBundleListsTheSupportReferencesOfItsFailureRecords(t *testing.T) {
	const installFailure = "support_598bfe61-4113-4390-b17c-a7a0c0bc1858"
	const laterFailure = "support_101c6c77-c969-4a91-96b3-2210a2bb3364"
	const shutdownInfo = "support_5b5b0743-c870-4e55-8f92-e0b137c3137b"
	persisted := []byte(`{"severity":"error","code":"unexpected_cli_failure","support_reference":"` + installFailure + `"}
{"severity":"info","code":"telemetry_shutdown","support_reference":"` + shutdownInfo + `"}
not json at all ` + laterFailure + `
{"severity":"error","support_reference":"not-a-reference"}
`)
	recent, err := NewEventWithSupportReference(time.Now(), "command", "command_failed", "error", laterFailure, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := NewEventWithSupportReference(time.Now(), "command", "unexpected_cli_failure", "error", installFailure, nil)
	got := failureReferences(persisted, []Event{recent, again})
	// Only error records count, only their structured field, each once with the newest occurrence last.
	if want := []string{laterFailure, installFailure}; !slices.Equal(got, want) {
		t.Fatalf("failure references %q, want %q", got, want)
	}
	var many []Event
	for i := 0; i < MaximumBundleFailureReferences+10; i++ {
		event, err := NewEventWithSupportReference(time.Now(), "command", "command_failed", "error", supportref.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		many = append(many, event)
	}
	if got := failureReferences(nil, many); len(got) != MaximumBundleFailureReferences || got[len(got)-1] != many[len(many)-1].SupportReference {
		t.Fatalf("bounded list: %d entries, last %q", len(got), got[len(got)-1])
	}
}
