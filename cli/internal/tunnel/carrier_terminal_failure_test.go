package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"io"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestTerminalRecoveryAtCarrierPreparationBoundaries(t *testing.T) {
	for _, phase := range []string{"before_offer", "after_grant", "after_attach", "after_ack"} {
		t.Run(phase, func(t *testing.T) { connectedProductionRuntime(t, "terminal_move", phase) })
	}
}

// Lose the replacement at actual protocol boundaries, retaining the real PTY,
// native transports, owner RPC, replay cursor and input decision queue. Recovery
// uses the same primary attach path as today's CLI reconnect.
func (f *connectedTerminalMove) exerciseFailure(t *testing.T, ctx context.Context, runtime *cliNativeRuntime, old *native.Owner, authority *tailnet.Authority, store config.ProfileStore, leaf tls.Certificate, public ed25519.PublicKey) {
	t.Helper()
	calls := 0
	application := peerApplication{helper: func(ctx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		calls++
		if calls == 2 && f.crashPhase == "after_grant" {
			f.candidateStop()
		}
		helper, err := newInitializedHelperTerminalConn(ctx, message, target, 4)
		if calls == 2 && f.crashPhase == "after_attach" {
			f.candidateStop()
		}
		return helper, err
	}}
	conn, err := runtime.openApplication(ctx, "machine_connected", "terminal", application, f.target, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ready := make([]byte, 8)
	if _, err := io.ReadFull(conn, ready); err != nil || !bytes.Equal(ready, []byte("READY000")) {
		t.Fatal("initial terminal was unavailable", err)
	}
	payload := bytes.Repeat([]byte("input-once-0123456789"), 128)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 31)
	if _, err := io.ReadFull(conn, prefix); err != nil || !bytes.Equal(prefix, payload[:31]) {
		t.Fatal("partial output before preparation changed", err)
	}
	carrier := conn.(*carrierTerminal)
	offer := f.parallelCarrier(t, ctx, authority, store, leaf, public)
	if f.crashPhase == "before_offer" {
		f.candidateStop()
	}
	moveErr := old.OfferCarrier(ctx, offer)
	if f.crashPhase == "before_offer" || f.crashPhase == "after_grant" {
		if moveErr == nil {
			t.Fatal("failed preparation was acknowledged as a completed move")
		}
	}
	if f.crashPhase == "after_ack" {
		if moveErr != nil {
			t.Fatal(moveErr)
		}
		f.candidateStop()
	}
	// Discard the failed transport's unread local queue. The committed delivery
	// cursor, not its producer cursor, determines exactly what must be replayed.
	cursor := int(f.deliveredCursor.Load())
	carrier.mu.Lock()
	target := *carrier.target
	carrier.mu.Unlock()
	conn.Close()
	target.AfterSequence, target.RestartIfNotRunning, target.ReplayHistory = cursor, false, false
	target.SequenceSink = func(value int) { f.deliveredCursor.Store(int64(value)) }
	recovery, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resumed, err := runtime.openApplication(recovery, "machine_connected", "terminal", peerApplication{helper: func(ctx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		return newInitializedHelperTerminalConn(ctx, message, target, 4)
	}}, &target, time.Now)
	if err != nil {
		t.Fatal("primary reconnect after replacement failure", err)
	}
	defer resumed.Close()
	rest := make([]byte, len(payload)-len(prefix))
	if _, err := io.ReadFull(resumed, rest); err != nil || !bytes.Equal(rest, payload[len(prefix):]) {
		t.Fatal("replacement failure lost or duplicated terminal output", err)
	}
	final := []byte("FINAL001")
	if _, err := resumed.Write(final); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(final))
	if _, err := io.ReadFull(resumed, got); err != nil || !bytes.Equal(got, final) {
		t.Fatal("replacement failure replayed terminal input", err)
	}
	if snapshot, err := f.sessions.Snapshot(f.target.SessionID); err != nil || snapshot.Generation != 1 {
		t.Fatal("replacement failure replaced the shell", err)
	}
}
