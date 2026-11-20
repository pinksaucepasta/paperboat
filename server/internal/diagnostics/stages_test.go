package diagnostics

import (
	"context"
	"testing"
	"time"
)

func TestStagesRecordConsecutiveSteps(t *testing.T) {
	ctx, stages := WithStages(context.Background())
	time.Sleep(15 * time.Millisecond)
	Stage(ctx, "read")
	time.Sleep(15 * time.Millisecond)
	Stage(ctx, "proof")
	attrs := stages.Attrs()
	if len(attrs) != 4 || attrs[0] != "read_ms" || attrs[2] != "proof_ms" {
		t.Fatalf("attrs = %v, want read_ms and proof_ms", attrs)
	}
	for _, i := range []int{1, 3} {
		if ms, ok := attrs[i].(int64); !ok || ms < 10 {
			t.Fatalf("attrs[%d] = %v, want a step of at least 10 ms", i, attrs[i])
		}
	}
	Stage(context.Background(), "ignored") // no Stages in the context: no panic, nothing recorded
}
