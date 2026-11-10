package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// victus 2026-11-10: interactive `pb ssh hp` ended at once with the generic failure text. The loopback connection
// overrides Hostname, so the managed fragment's `Match host <alias> user <user>` group (which offers the managed key)
// did not apply and ssh.exe had no identity to offer. The managed user's identity is passed on the command line.
func TestWindowsLoopbackOpenSSHArgumentsOfferTheManagedIdentity(t *testing.T) {
	identity := managedssh.ManagedIdentityArguments(`\\.\pipe\Paperboat-SSH-Agent`, `C:\Users\Pujan\.ssh\paperboat_managed_identity.pub`)
	destination := managedssh.Destination{User: "root", Host: "hn.local.example.test", Port: 2222, Managed: true}
	got := windowsLoopbackOpenSSHArguments(destination, 49152, `C:\Program Files\Paperboat\pb.exe`, identity, []string{"/usr/bin/printf", "VICTUS_PB_SSH_OK"}, true)
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", `IdentityAgent="\\\\.\\pipe\\Paperboat-SSH-Agent"`,
		"-o", `IdentityFile="C:\\Users\\Pujan\\.ssh\\paperboat_managed_identity.pub"`,
		"-o", "ProxyCommand=none",
		"-o", "Hostname=127.0.0.1",
		"-o", "HostKeyAlias=hn.local.example.test",
		"-o", `KnownHostsCommand="C:\Program Files\Paperboat\pb.exe" __ssh-known-hosts --host hn.local.example.test --port 2222`,
		"-p", "49152",
		"root@hn.local.example.test",
		"/usr/bin/printf", "VICTUS_PB_SSH_OK",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("windowsLoopbackOpenSSHArguments()=%q want %q", got, want)
	}
	// Any other OS user authenticates with their own credentials: the managed key is never offered to them.
	destination.Managed = false
	if other := strings.Join(windowsLoopbackOpenSSHArguments(destination, 49152, `C:\pb.exe`, identity, nil, false), "\n"); strings.Contains(other, "Identity") || strings.Contains(other, "BatchMode") {
		t.Fatalf("another user is offered the managed identity: %q", other)
	}
}

func TestWindowsLoopbackOpenSSHArgumentsKeepDefaultRegisteredPortSeparateFromLoopback(t *testing.T) {
	destination := managedssh.Destination{User: "root", Host: "hn.local.example.test", Port: 22}
	got := windowsLoopbackOpenSSHArguments(destination, 50443, `C:\Program Files\Paperboat\pb.exe`, nil, []string{"must-not-appear"}, false)
	joined := strings.Join(got, "\n")
	for _, required := range []string{
		`KnownHostsCommand="C:\Program Files\Paperboat\pb.exe" __ssh-known-hosts --host hn.local.example.test --port 22`,
		"HostKeyAlias=hn.local.example.test",
		"ProxyCommand=none",
		"Hostname=127.0.0.1",
		"50443",
		"root@hn.local.example.test",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("arguments %q do not contain %q", got, required)
		}
	}
	if strings.Contains(joined, "must-not-appear") {
		t.Fatalf("passthrough appended without --: %q", got)
	}
}
