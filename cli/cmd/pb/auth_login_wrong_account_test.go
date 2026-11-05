package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

// pbmac 2026-11-05: pb login --reauth approved by a different account ended with only "Paperboat could not
// finish the command". The refusal is right; the message must name both accounts and the way out.
func TestReauthApprovedByAnotherAccountNamesBothAccounts(t *testing.T) {
	f := &loginDeviceFixture{}
	srv := loginDeviceServer(t, f)
	oldMachine, oldPair := loginLocalMachineID, pairLoginDevice
	t.Cleanup(func() { loginLocalMachineID, pairLoginDevice = oldMachine, oldPair })
	loginLocalMachineID = func(*config.Config) (string, error) { return "machine_abc", nil }
	pairLoginDevice = func(*command.Context, string, *config.LoginEnrollment, string) error { return nil }
	path, _ := browserLoginFixture(t, srv.URL)
	if out, err := runBrowserLogin(t, context.Background(), path, "login", "--no-browser"); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	f.approvedBy = "other@example.test"
	_, err := runBrowserLogin(t, context.Background(), path, "login", "--reauth", "--no-browser")
	if err == nil {
		t.Fatal("reauth approved by another account succeeded")
	}
	shown := commandFailureMessage(err, classifyCommandFailure(err))
	for _, want := range []string{"browser@example.test", "other@example.test", "pb login --change-account"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("shown message lacks %q: %q", want, shown)
		}
	}
}
