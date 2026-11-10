package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestKeptWindowsSSHPortKeepsTheRegisteredPortOfTheSameUser(t *testing.T) {
	registered := identity.Registration{SSHUser: "alice", SSHPort: 58965}
	if got := keptWindowsSSHPort(registered, "alice", 38222); got != 58965 {
		t.Fatalf("the registered port was replaced by the default: %d", got)
	}
	if got := keptWindowsSSHPort(registered, "bob", 38222); got != 38222 {
		t.Fatalf("another user's port was kept: %d", got)
	}
	if got := keptWindowsSSHPort(identity.Registration{SSHUser: "alice"}, "alice", 38222); got != 38222 {
		t.Fatalf("a registration without a port replaced the default: %d", got)
	}
}
