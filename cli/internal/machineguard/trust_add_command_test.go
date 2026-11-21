package machineguard

import (
	"slices"
	"strings"
	"testing"
)

func TestDarwinTrustAddRunsInConsoleUserSession(t *testing.T) {
	name, args, ok := darwinTrustAddCommand(501, "/s/root.pem")
	if !ok || name != "/bin/launchctl" || !slices.Equal(args[:3], []string{"asuser", "501", "/usr/bin/security"}) || args[len(args)-1] != "/s/root.pem" {
		t.Fatalf("console session: %v %q %v", ok, name, args)
	}
	if _, _, ok := darwinTrustAddCommand(0, "/s/root.pem"); ok {
		t.Fatal("headless Mac (console uid 0) must not try a prompt nobody can see")
	}
	if msg := darwinTrustHeadless("/s/root.pem").Error(); !strings.Contains(msg, "sudo security add-trusted-cert -d -r trustRoot -p ssl -k /Library/Keychains/System.keychain /s/root.pem") {
		t.Fatalf("headless message lacks the exact command: %s", msg)
	}
}
