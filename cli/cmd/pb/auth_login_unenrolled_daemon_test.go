package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// pbmac 2026-11-05: the CLI session was valid but the daemon said awaiting_enrollment, and pb login answered
// "Already signed in" (and --reauth could not fix it). It must say the service is not enrolled and point at --fresh.
func TestLoginOnASignedInComputerWhoseDaemonIsNotEnrolledPointsAtFresh(t *testing.T) {
	f := &loginDeviceFixture{}
	srv := loginDeviceServer(t, f)
	oldMachine, oldPair, oldDaemon := loginLocalMachineID, pairLoginDevice, loginDaemonAwaitingEnrollment
	t.Cleanup(func() {
		loginLocalMachineID, pairLoginDevice, loginDaemonAwaitingEnrollment = oldMachine, oldPair, oldDaemon
	})
	loginLocalMachineID = func(*config.Config) (string, error) { return "machine_abc", nil }
	pairLoginDevice = func(*command.Context, string, *config.LoginEnrollment, string) error { return nil }
	loginDaemonAwaitingEnrollment = func(context.Context) bool { return false }
	path, _ := browserLoginFixture(t, srv.URL)
	if out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser"); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	loginDaemonAwaitingEnrollment = func(context.Context) bool { return true }
	again, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser")
	if err == nil || strings.Contains(again+err.Error(), "Already signed in") || !strings.Contains(err.Error(), "pb login --fresh") || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("login with an unenrolled daemon: err=%v output=%s", err, again)
	}
}
