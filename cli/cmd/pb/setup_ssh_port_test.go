package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestKeptWindowsSSHTargetKeepsTheRegisteredUserAndPort(t *testing.T) {
	registered := identity.Registration{SSHUser: "runneradmin", SSHPort: 58965}
	if user, port := keptWindowsSSHTarget(registered, `HOST\runneradmin`, 38222); user != "runneradmin" || port != 58965 {
		t.Fatalf("the registered target was replaced: %q %d", user, port)
	}
	if user, port := keptWindowsSSHTarget(identity.Registration{SSHUser: "alice"}, "bob", 38222); user != "bob" || port != 38222 {
		t.Fatalf("a registration without a port replaced the defaults: %q %d", user, port)
	}
	if user, port := keptWindowsSSHTarget(identity.Registration{SSHPort: 58965}, "bob", 38222); user != "bob" || port != 38222 {
		t.Fatalf("a registration without a user replaced the defaults: %q %d", user, port)
	}
}
