//go:build darwin

package deviceguard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDarwinGuardAccountInterruptedCreation(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_DARWIN_TEST") != "1" {
		t.Skip("requires authorized macOS account target")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	name := fmt.Sprintf("_pb_review_%d", os.Getpid())
	path := "/Users/" + name
	if exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path).Run() == nil {
		t.Fatal("test account occupied")
	}
	if _, err := darwinRun(ctx, "", "/usr/bin/dscl", ".", "-create", path, "RealName", "Paperboat protected listener"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := darwinRun(cleanup, "", "/usr/bin/dscl", ".", "-delete", path); err != nil {
			t.Error(err)
		}
	}()
	// Resume after ownership marking but before UniqueID assignment.
	if err := darwinInstallAccount(ctx, name); err != nil {
		t.Fatal(err)
	}
	before, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path, "UniqueID").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = darwinInstallAccount(ctx, name); err != nil {
		t.Fatal(err)
	}
	after, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path, "UniqueID").Output()
	if err != nil || string(before) != string(after) {
		t.Fatal("account identity changed on retry", err)
	}
	for _, attribute := range []struct{ name, want string }{{"UserShell", "/usr/bin/false"}, {"NFSHomeDirectory", "/var/empty"}, {"AuthenticationAuthority", ";DisabledUser;"}} {
		output, err := exec.CommandContext(ctx, "/usr/bin/dscl", ".", "-read", path, attribute.name).Output()
		if err != nil || !strings.Contains(string(output), attribute.want) {
			t.Fatalf("account %s not disabled: %v", attribute.name, err)
		}
	}
}
