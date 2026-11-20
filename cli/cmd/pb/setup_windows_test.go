//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
)

func TestSetupOpenSSHSkipsOnlyWhatMeansUnobtainableOrOutOfTime(t *testing.T) {
	var notice bytes.Buffer
	withNotice := withDependencies(context.Background(), defaultWindowsSetupDependencies, func(d *windowsSetupDependencies) { d.noticeWriter = &notice })
	budget := withTimings(withNotice, func(timings *commandTimings) { timings.setupOpenSSH = 30 * time.Millisecond })

	unobtainable := &elevation.RemoteError{Code: elevation.CodeOpenSSHUnobtainable, Message: "no package"}
	if err := runSetupOpenSSH(budget, func(context.Context) error { return unobtainable }); err != nil {
		t.Fatalf("an unobtainable package was not skipped: %v", err)
	}
	for _, want := range []string{"Managed SSH was not prepared", "setup continues without it", "pb doctor --repair"} {
		if !strings.Contains(notice.String(), want) {
			t.Fatalf("the notice lacks %q: %q", want, notice.String())
		}
	}
	if err := runSetupOpenSSH(budget, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }); err != nil {
		t.Fatalf("the budget's expiry was not skipped: %v", err)
	}
	refusal := &elevation.RemoteError{Code: "operation_failed", Message: "untrusted binary"}
	if err := runSetupOpenSSH(budget, func(context.Context) error { return refusal }); !errors.Is(err, elevation.ErrElevatedOperation) {
		t.Fatalf("another elevated failure was skipped: %v", err)
	}
	if err := runSetupOpenSSH(budget, func(ctx context.Context) error { <-ctx.Done(); return refusal }); !errors.Is(err, elevation.ErrElevatedOperation) {
		t.Fatalf("a refusal after the budget was skipped: %v", err)
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSetupOpenSSH(parent, func(ctx context.Context) error { return ctx.Err() }); err == nil {
		t.Fatal("the caller's own cancellation was skipped")
	}
}
