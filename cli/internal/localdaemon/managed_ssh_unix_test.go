//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func TestManagedSSHRuntimeRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := StartManagedSSH(context.Background(), ManagedSSHConfig{}); !errors.Is(err, ErrInvalidInventoryConfig) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagedSSHHealthCodeIsTyped(t *testing.T) {
	if code := ManagedSSHHealthCode(nil); code != "" {
		t.Fatalf("healthy code=%q", code)
	}
	if code := ManagedSSHHealthCode(managedssh.ErrOpenSSHUnavailable); code != "ssh_target_not_ready" {
		t.Fatalf("OpenSSH code=%q", code)
	}
	if code := ManagedSSHHealthCode(errors.New("unknown operational failure")); code != "ssh_target_not_ready" {
		t.Fatalf("unknown failure code=%q", code)
	}
}

func TestManagedSSHProxyPreservesQualifiedWindowsUsername(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is unavailable")
	}
	root := t.TempDir()
	proxy, capture := filepath.Join(root, "proxy.sh"), filepath.Join(root, "username")
	script := "#!/bin/sh\nprintf '%s' \"$7\" > " + strconv.Quote(capture) + "\n"
	if err := os.WriteFile(proxy, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// OpenSSH rejects qualified users supplied on its command line before
	// invoking any proxy. Literal User configuration is its supported form.
	config := filepath.Join(root, "ssh_config")
	if err := os.WriteFile(config, []byte("Host fixture.example.test\n    User VICTUS\\Pujan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, ssh, "-F", config, "-o", "ProxyCommand="+managedSSHProxyCommand(strconv.Quote(proxy)), "-o", "ConnectTimeout=2", "fixture.example.test")
	// The capture proxy deliberately ends before SSH authentication.
	_ = command.Run()
	if ctx.Err() != nil {
		t.Fatal("OpenSSH proxy did not finish within its deadline")
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != `VICTUS\Pujan` {
		t.Fatalf("proxy changed qualified username: %q, %v", got, err)
	}
}
