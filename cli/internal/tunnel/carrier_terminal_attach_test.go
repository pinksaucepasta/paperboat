package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"io"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// dadape direct2d: output stalled 4.85 s while the replacement attached, because
// the old reader was cut before the new attach. Output the PTY produces while the
// replacement is attaching must arrive on the old link, exactly once.
func TestTerminalOutputFlowsWhileReplacementAttaches(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires a Unix PTY")
	}
	connectedProductionRuntime(t, "terminal_move", "output_during_attach")
}

func (f *connectedTerminalMove) exerciseOutputDuringAttach(t *testing.T, ctx context.Context, runtime *cliNativeRuntime, old *native.Owner, authority *tailnet.Authority, store config.ProfileStore, leaf tls.Certificate, public ed25519.PublicKey) {
	t.Helper()
	var conn Conn
	calls := 0
	during := make(chan error, 1)
	application := peerApplication{helper: func(ctx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		calls++
		if calls == 2 {
			// The replacement is attaching. Produce output from the host side and
			// require the local reader to receive it before the attach goes on.
			carrier := conn.(*carrierTerminal)
			carrier.mu.Lock()
			attachment, generation := carrier.helper.attachmentID, carrier.helper.generation
			carrier.mu.Unlock()
			if err := f.sessions.WriteStream(f.target.SessionID, attachment, generation, []byte("MID00001")); err != nil {
				during <- err
			} else {
				got := make([]byte, 8)
				read := make(chan error, 1)
				go func() {
					_, err := io.ReadFull(conn, got)
					read <- err
				}()
				select {
				case err := <-read:
					if err == nil && !bytes.Equal(got, []byte("MID00001")) {
						err = io.ErrUnexpectedEOF
					}
					during <- err
				case <-time.After(3 * time.Second):
					during <- context.DeadlineExceeded
					<-read
				}
			}
		}
		return newInitializedHelperTerminalConn(ctx, message, target, 4)
	}}
	var err error
	conn, err = runtime.openApplication(ctx, "machine_connected", "terminal", application, f.target, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ready := make([]byte, 8)
	if _, err := io.ReadFull(conn, ready); err != nil || !bytes.Equal(ready, []byte("READY000")) {
		t.Fatal("initial terminal was unavailable", err)
	}
	offer := f.parallelCarrier(t, ctx, authority, store, leaf, public)
	if err := old.OfferCarrier(ctx, offer); err != nil {
		t.Fatal("planned terminal move failed", err)
	}
	if err := <-during; err != nil {
		t.Fatal("output stalled while the replacement attached", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	// The replacement replays from its attach cursor; output the old link
	// already delivered must not appear twice.
	tail := []byte("FINAL001")
	if _, err := conn.Write(tail); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(tail))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, tail) {
		t.Fatalf("output after the move was %q, want %q: %v", got, tail, err)
	}
}
