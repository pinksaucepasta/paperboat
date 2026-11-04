//go:build linux || darwin

package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
)

// The update's primary leg (ProductionCarrier.MoveTerminals is
// c.peer.current.owner.OfferCarrier) must move a terminal held on the primary
// carrier to the generation the real session owner staged, through the real
// prepare-peer bridge, before the old carrier stops (run24 never moved).
func TestPrimaryCarrierMovesHeldTerminalToStagedGeneration(t *testing.T) {
	if !targetCarrierRoot(t) {
		return
	}
	f := newTargetCarrierFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.owner.sessions.Create(ctx, session.CreateRequest{ID: "ses_primary_move", Name: "primary-move", Command: pty.Command{Path: shell, Args: []string{"-c", "stty -echo; exec cat"}, CWD: f.root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: 1})
	if err != nil || len(ready.Offer) == 0 {
		t.Fatal("session owner did not stage a generation", err)
	}
	offer, err := native.ParseCarrierOffer(ready.Offer)
	if err != nil {
		t.Fatal(err)
	}
	f.ensureNativePrimary(t, ctx)
	parent, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	f.parent = parent
	f.admit(t, ctx, parent, created)
	moved := make(chan *native.Session, 1)
	if err := parent.OnCarrierMove(func(moveCtx context.Context, offered native.CarrierOffer) error {
		next, err := parent.DialCarrier(moveCtx, offered)
		if err != nil {
			t.Log("client move failed:", err)
			return err
		}
		moved <- next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := f.primaryOwner.OfferCarrier(ctx, offer); err != nil {
		t.Fatal("primary leg declined the staged generation", err)
	}
	t.Log("primary leg move took", time.Since(started))
	var next *native.Session
	select {
	case next = <-moved:
	default:
		t.Fatal("primary leg returned success without moving the held terminal")
	}
	defer next.Close()
	f.admit(t, ctx, next, created)
}
