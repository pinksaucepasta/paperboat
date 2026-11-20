package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// a test Mac 2026-11-05: the CLI session was valid but the daemon said awaiting_enrollment, and pb login answered
// "Already signed in" (and --reauth could not fix it). It must ask for an approval again (that is how a removed
// computer is added back), never "Already signed in".
func TestLoginOnASignedInComputerWhoseDaemonIsNotEnrolledAsksForApprovalAgain(t *testing.T) {
	f := &loginDeviceFixture{}
	srv := loginDeviceServer(t, f)
	awaiting := false
	ctx := loginDeviceContext(t, srv, func(d *authDependencies) {
		d.localMachineID = func(*config.Config) (string, error) { return "machine_abc", nil }
		d.pairLoginDevice = func(*command.Context, string, *config.LoginEnrollment, string) error { return nil }
		d.daemonAwaitingEnrollment = func(context.Context) bool { return awaiting }
	})
	path, _ := browserLoginFixture(t, srv.URL)
	if out, err := runBrowserLogin(t, ctx, path, "login", "--no-browser"); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	awaiting = true
	f.authorize = nil
	again, err := runBrowserLogin(t, ctx, path, "login", "--no-browser")
	if err != nil || strings.Contains(again, "Already signed in") || f.authorize["enrolled_machine_id"] != "machine_abc" {
		t.Fatalf("login with an unenrolled daemon: err=%v authorize=%v output=%s", err, f.authorize, again)
	}
}
