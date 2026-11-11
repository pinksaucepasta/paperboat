//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// pbvm1 F14-03: no hostd ran and the machine was offline, yet pb doctor reported healthy.
func TestDoctorFailsWhenTheInstalledHostServiceDoesNotAnswer(t *testing.T) {
	installed := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	refused := func(context.Context, string) error { return errors.New("connect: connection refused") } // a stale socket file
	if got := doctorHostdCheck(t.Context(), installed, "/run/stale.sock", refused, 20*time.Millisecond, time.Millisecond); got.Status != doctorpkg.StatusFail || got.Recovery == "" {
		t.Fatalf("a missing host service reported %+v", got)
	}
	answering := func(context.Context, string) error { return nil }
	if got := doctorHostdCheck(t.Context(), installed, "/run/hostd.sock", answering, 20*time.Millisecond, time.Millisecond); got.Status != doctorpkg.StatusPass {
		t.Fatalf("a serving host service reported %+v", got)
	}
	if got := doctorHostdCheck(t.Context(), filepath.Join(t.TempDir(), "absent"), "/run/none.sock", refused, 20*time.Millisecond, time.Millisecond); got.Status != doctorpkg.StatusPass {
		t.Fatalf("a computer without the native installation reported %+v", got)
	}
}

// A hostd restart or hand-over refuses connections for a moment: interactive `pb doctor` must not report it as
// not running when it answers inside the window.
func TestDoctorWaitsOutAHostdRestart(t *testing.T) {
	installed := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	restarting := func(context.Context, string) error {
		if calls++; calls < 4 {
			return errors.New("connect: connection refused")
		}
		return nil
	}
	if got := doctorHostdCheck(t.Context(), installed, "/run/hostd.sock", restarting, time.Second, time.Millisecond); got.Status != doctorpkg.StatusPass || calls != 4 {
		t.Fatalf("a restart inside the window reported %+v after %d dials", got, calls)
	}
}

// Doctor stops waiting for a restarting hostd the moment its own context ends, not after the retry interval.
func TestDoctorHostdWaitEndsWithTheContext(t *testing.T) {
	installed := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	refused := func(context.Context, string) error { return errors.New("connect: connection refused") }
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := doctorHostdCheck(ctx, installed, "/run/stale.sock", refused, time.Minute, 10*time.Second)
	if got.Status != doctorpkg.StatusFail || time.Since(started) > 2*time.Second {
		t.Fatalf("status %v after %v: the wait slept through its context", got.Status, time.Since(started))
	}
}
