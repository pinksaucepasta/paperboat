//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// pbvm1 F14-03: no hostd ran and the machine was offline, yet pb doctor reported healthy.
func TestDoctorFailsWhenTheInstalledHostServiceDoesNotAnswer(t *testing.T) {
	installed := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	refused := func(context.Context, string) error { return errors.New("connect: connection refused") } // a stale socket file
	if got := doctorHostdCheck(t.Context(), installed, "/run/stale.sock", refused); got.Status != doctorpkg.StatusFail || got.Recovery == "" {
		t.Fatalf("a missing host service reported %+v", got)
	}
	answering := func(context.Context, string) error { return nil }
	if got := doctorHostdCheck(t.Context(), installed, "/run/hostd.sock", answering); got.Status != doctorpkg.StatusPass {
		t.Fatalf("a serving host service reported %+v", got)
	}
	if got := doctorHostdCheck(t.Context(), filepath.Join(t.TempDir(), "absent"), "/run/none.sock", refused); got.Status != doctorpkg.StatusPass {
		t.Fatalf("a computer without the native installation reported %+v", got)
	}
}
