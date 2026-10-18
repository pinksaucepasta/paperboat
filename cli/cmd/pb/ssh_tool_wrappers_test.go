package main

import (
	"flag"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

const managedToolTestDomain = "machines.example.test"

func managedToolTestContext() *command.Context {
	return command.NewContext(flag.NewFlagSet("managed-tool", flag.ContinueOnError))
}

func managedToolTestResolver(calls *[]string) managedToolTargetResolver {
	return func(ctx *command.Context, target, requestedUser string) (managedssh.Destination, error) {
		*calls = append(*calls, target+"@"+requestedUser)
		user := requestedUser
		if user == "" {
			user = "adam"
		}
		return managedssh.Destination{Alias: target, Host: target + "." + managedToolTestDomain, Port: 22, User: user}, nil
	}
}

func TestRewriteManagedSCPOperandsPreservesFlagsAndLocalPaths(t *testing.T) {
	var calls []string
	input := []string{"-q", "-o", "ProxyCommand=helper:x", "./local:file", "mac:/tmp/file"}
	got, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", input, managedToolTestDomain, managedToolTestResolver(&calls))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-q", "-o", "ProxyCommand=helper:x", "./local:file", "adam@mac.machines.example.test:/tmp/file"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(calls, []string{"mac@"}) {
		t.Fatalf("rewrite=%q calls=%q, want %q", got, calls, want)
	}
	if !reflect.DeepEqual(input, []string{"-q", "-o", "ProxyCommand=helper:x", "./local:file", "mac:/tmp/file"}) {
		t.Fatalf("input mutated: %q", input)
	}
}

func TestRewriteManagedToolsApplyExplicitUserAndNamespace(t *testing.T) {
	var calls []string
	resolver := managedToolTestResolver(&calls)
	scp, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", []string{"payload", "bob@mac.machines.example.test:dest"}, managedToolTestDomain, resolver)
	if err != nil || !reflect.DeepEqual(scp, []string{"payload", "bob@mac.machines.example.test:dest"}) {
		t.Fatalf("scp=%q error=%v", scp, err)
	}
	sftp, err := rewriteManagedToolArguments(managedToolTestContext(), "sftp", []string{"-o", "ConnectTimeout=5", "mac"}, managedToolTestDomain, resolver)
	if err != nil || !reflect.DeepEqual(sftp, []string{"-o", "ConnectTimeout=5", "adam@mac.machines.example.test"}) {
		t.Fatalf("sftp=%q error=%v", sftp, err)
	}
	rsync, err := rewriteManagedToolArguments(managedToolTestContext(), "rsync", []string{"-a", "bob@mac:src", "C:\\local\\dest"}, managedToolTestDomain, resolver)
	if err != nil || !reflect.DeepEqual(rsync, []string{"-a", "bob@mac.machines.example.test:src", "C:\\local\\dest"}) {
		t.Fatalf("rsync=%q error=%v", rsync, err)
	}
	wantCalls := []string{"mac@bob", "mac@", "mac@bob"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%q want=%q", calls, wantCalls)
	}
}

func TestRewriteManagedToolRejectsMissingRemoteOperand(t *testing.T) {
	var calls []string
	if _, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", []string{"local-a", "local-b"}, managedToolTestDomain, managedToolTestResolver(&calls)); err == nil {
		t.Fatal("missing remote operand accepted")
	}
	if len(calls) != 0 {
		t.Fatalf("unexpected resolution: %q", calls)
	}
}

func TestRewriteManagedToolUsesCanonicalMachineSuffix(t *testing.T) {
	var target string
	resolver := func(_ *command.Context, value, user string) (managedssh.Destination, error) {
		target = value
		return managedssh.Destination{Host: value + "." + managedToolTestDomain, User: user, Port: 22}, nil
	}
	got, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", []string{"file", "bob@studio.machines.example.test:/tmp"}, managedToolTestDomain, resolver)
	if err != nil || target != "studio" || !reflect.DeepEqual(got, []string{"file", "bob@studio.machines.example.test:/tmp"}) {
		t.Fatalf("rewrite=%q target=%q err=%v", got, target, err)
	}
}

func TestRewriteManagedSFTPDestinationPreservesTransferPaths(t *testing.T) {
	for _, test := range []struct {
		name            string
		arguments, want []string
		resolution      string
	}{
		{
			name:       "remote path",
			arguments:  []string{"mac:/tmp/remote file"},
			want:       []string{"adam@mac.machines.example.test:/tmp/remote file"},
			resolution: "mac@",
		},
		{
			name:       "download",
			arguments:  []string{"-b", "batch:commands", "bob@mac:/tmp/remote file", "./local:file"},
			want:       []string{"-b", "batch:commands", "bob@mac.machines.example.test:/tmp/remote file", "./local:file"},
			resolution: "mac@bob",
		},
		{
			name:       "option terminator",
			arguments:  []string{"--", "mac", "./local-file"},
			want:       []string{"--", "adam@mac.machines.example.test", "./local-file"},
			resolution: "mac@",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			got, err := rewriteManagedToolArguments(managedToolTestContext(), "sftp", test.arguments,
				managedToolTestDomain, managedToolTestResolver(&calls))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) || !reflect.DeepEqual(calls, []string{test.resolution}) {
				t.Fatalf("rewrite=%q calls=%q, want %q and resolution %q", got, calls, test.want, test.resolution)
			}
		})
	}
}
