package localdaemon

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// F3-15: after a full redial the same pb connect keeps its unanswered input: same queue, same attachment.
func TestInputOwnerKeepsQueueAndAttachmentAcrossRedial(t *testing.T) {
	owners := &inputOwners{entries: make(map[inputOwnerKey]inputOwnerEntry)}
	now := time.Now()
	first := &resolver.TerminalTarget{SessionID: "ses_1"}
	owners.adopt("owner_a", "machine_1", first, now)
	if _, err := first.InputQueue.Enqueue([]byte("keys")); err != nil {
		t.Fatal(err)
	}
	first.InputAttachmentID = "att_1" // set by the helper on attach
	second := &resolver.TerminalTarget{SessionID: "ses_1"}
	owners.adopt("owner_a", "machine_1", second, now.Add(time.Second))
	if second.InputQueue != first.InputQueue || second.InputAttachmentID != "att_1" {
		t.Fatalf("queue reused=%v attachment=%q", second.InputQueue == first.InputQueue, second.InputAttachmentID)
	}
	other := &resolver.TerminalTarget{SessionID: "ses_2"}
	owners.adopt("owner_a", "machine_1", other, now.Add(time.Second))
	stranger := &resolver.TerminalTarget{SessionID: "ses_1"}
	owners.adopt("owner_b", "machine_1", stranger, now.Add(time.Second))
	if other.InputQueue == first.InputQueue || stranger.InputQueue == first.InputQueue || stranger.InputAttachmentID != "" {
		t.Fatal("input state crossed sessions or owners")
	}
	late := &resolver.TerminalTarget{SessionID: "ses_1"}
	owners.adopt("owner_a", "machine_1", late, now.Add(inputOwnerTTL+2*time.Second))
	if late.InputQueue == first.InputQueue {
		t.Fatal("an expired owner was reused")
	}
}
