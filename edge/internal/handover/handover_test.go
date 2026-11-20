package handover

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// No serving process: a fresh start inherits nothing.
func TestInheritWithoutAServingProcessIsAFreshStart(t *testing.T) {
	inheritance, err := Inherit(t.TempDir())
	if err != nil || inheritance != nil {
		t.Fatalf("inherit = %v, %v; want a fresh start", inheritance, err)
	}
}

var _ net.Listener = (*listener)(nil)

func TestRemainingAfterTakeover(t *testing.T) {
	defer tookOverAt.Store(tookOverAt.Load())
	tookOverAt.Store(0)
	if got := RemainingAfterTakeover(time.Minute); got != 0 {
		t.Fatalf("without a takeover = %v, want 0", got)
	}
	tookOverAt.Store(time.Now().Add(-4 * time.Second).UnixNano())
	if got := RemainingAfterTakeover(10 * time.Second); got < 5*time.Second || got > 6*time.Second {
		t.Fatalf("4 s into a 10 s window = %v, want about 6 s", got)
	}
	if got := RemainingAfterTakeover(3 * time.Second); got != 0 {
		t.Fatalf("after the window = %v, want 0", got)
	}
}

// A serving process that sends an unreadable release reply must leave the
// replacement inactive and expose the decode cause to startup diagnostics.
func TestReadyPreservesMalformedReleaseCause(t *testing.T) {
	addr := &net.UnixAddr{Name: filepath.Join(t.TempDir(), "release.sock"), Net: "unixpacket"}
	listener, err := net.ListenUnix(addr.Net, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	replacement, err := net.DialUnix(addr.Net, nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	serving, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Close()
	if _, err := serving.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	inheritance := &Inheritance{conn: replacement}
	err = inheritance.Ready(ctx)
	var syntax *json.SyntaxError
	if !errors.Is(err, ErrHandover) || !errors.As(err, &syntax) {
		t.Fatalf("ready error = %v, want handover error wrapping the release reply decode cause", err)
	}
}
