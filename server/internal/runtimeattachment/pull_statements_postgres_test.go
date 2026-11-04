package runtimeattachment

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// F21-45: an edge's carrier pull and its observation ack read a constant number of statements per page, not
// per admission or observation (400 observations here, none of them a known carrier).
func TestEdgeCarrierPullAndAckStatementsAreConstantOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "stmts")
	if _, err := f.service.Attach(f.ctx, f.proof, f.request); err != nil {
		t.Fatal(err)
	}
	var statements atomic.Int64
	f.database.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		statements.Add(1)
		return ctx, func(string) {}
	})
	statements.Store(0)
	if _, err := f.service.Pull(f.ctx, f.edge, nil); err != nil {
		t.Fatal(err)
	}
	if got := statements.Load(); got > 3 {
		t.Fatalf("pull ran %d statements, want a constant handful", got)
	}
	page := make([]runtimewire.Observation, 400)
	for i := range page {
		page[i] = runtimewire.Observation{RouteID: fmt.Sprintf("%s_unknown_%d", f.prefix, i), SessionID: "session_x", AttachmentGeneration: 1, Ready: true}
	}
	statements.Store(0)
	if err := f.service.Observe(f.ctx, f.edge, page); !errors.Is(err, ErrStale) {
		t.Fatalf("observing unknown carriers = %v, want stale", err)
	}
	if got := statements.Load(); got > 6 {
		t.Fatalf("observing %d carriers ran %d statements, want a constant handful", len(page), got)
	}
}
