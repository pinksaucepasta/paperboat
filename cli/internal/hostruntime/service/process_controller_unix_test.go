//go:build !windows

package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func sleeper() *exec.Cmd { return exec.Command("sleep", "30") }

func TestProcessControllerStartsOnceStopsAndRestartsAfterExit(t *testing.T) {
	ctx := context.Background()
	starts := 0
	controller := &ProcessController{Command: func() *exec.Cmd { starts++; return sleeper() }}
	if status, _ := controller.Inspect(ctx, ""); status.Running || status.Registered {
		t.Fatalf("a controller that never started reports %#v", status)
	}
	if err := controller.Apply(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(ctx, ""); err != nil || starts != 1 {
		t.Fatalf("Start on a running child started again: err=%v starts=%d", err, starts)
	}
	if status, _ := controller.Inspect(ctx, ""); !status.Running || !status.Registered || !status.Ready {
		t.Fatalf("running child reports %#v", status)
	}
	if err := controller.Stop(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if status, _ := controller.Inspect(ctx, ""); status.Running {
		t.Fatalf("stopped child still reports running: %#v", status)
	}
	// A child that exits on its own is not running, so the next start request begins a new one.
	if err := controller.Apply(ctx, "", false); err != nil || starts != 2 {
		t.Fatalf("restart after stop: err=%v starts=%d", err, starts)
	}
	controller.cmd.Process.Kill()
	<-controller.done
	if err := controller.Start(ctx, ""); err != nil || starts != 3 {
		t.Fatalf("restart after a crash: err=%v starts=%d", err, starts)
	}
	if err := controller.Stop(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

func TestProcessControllerUpgradeReplacesTheRunningChildAndRemoveDeletesTheDeclaration(t *testing.T) {
	ctx := context.Background()
	controller := &ProcessController{Command: sleeper}
	if err := controller.Apply(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	first := controller.cmd.Process.Pid
	if err := controller.Apply(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if controller.cmd.Process.Pid == first {
		t.Fatal("an upgrading apply kept the old child")
	}
	definition := filepath.Join(t.TempDir(), "unit")
	if err := os.WriteFile(definition, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := controller.Remove(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(definition); !os.IsNotExist(err) {
		t.Fatalf("declaration left behind: %v", err)
	}
	if status, _ := controller.Inspect(ctx, ""); status.Running {
		t.Fatal("removed service still running")
	}
}

func TestProcessControllerKillsAChildThatIgnoresTermWithinTheCallersDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	controller := &ProcessController{Command: func() *exec.Cmd {
		return exec.Command("sh", "-c", `trap '' TERM; while :; do sleep 1; done`)
	}}
	if err := controller.Start(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := controller.Stop(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if status, _ := controller.Inspect(context.Background(), ""); status.Running {
		t.Fatal("a child that ignores TERM survived the stop deadline")
	}
}

func TestProcessControllerWithoutACommandIsInvalid(t *testing.T) {
	if err := (&ProcessController{}).Start(context.Background(), ""); err != ErrInvalidDefinition {
		t.Fatalf("Start = %v", err)
	}
}

func TestProcessControllerShutdownStopsTheChildAndRefusesLaterStarts(t *testing.T) {
	ctx := context.Background()
	controller := &ProcessController{Command: sleeper}
	if err := controller.Start(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if status, _ := controller.Inspect(ctx, ""); status.Running {
		t.Fatal("child survived Shutdown")
	}
	for name, start := range map[string]func() error{
		"Start": func() error { return controller.Start(ctx, "") },
		"Apply": func() error { return controller.Apply(ctx, "", false) },
	} {
		if err := start(); !errors.Is(err, ErrProcessShutdown) {
			t.Fatalf("%s after Shutdown = %v", name, err)
		}
	}
}
