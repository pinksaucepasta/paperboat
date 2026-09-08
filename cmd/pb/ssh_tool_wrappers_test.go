package main

import (
	"flag"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

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
		return managedssh.Destination{Alias: target, Host: target + ".pprbt", Port: 22, User: user}, nil
	}
}

func TestRewriteManagedSCPOperandsPreservesFlagsAndLocalPaths(t *testing.T) {
	var calls []string
	input := []string{"-q", "-o", "ProxyCommand=helper:x", "./local:file", "mac:/tmp/file"}
	got, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", input, managedToolTestResolver(&calls))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-q", "-o", "ProxyCommand=helper:x", "./local:file", "adam@mac.pprbt:/tmp/file"}
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
	scp, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", []string{"payload", "bob@mac.pprbt:dest"}, resolver)
	if err != nil || !reflect.DeepEqual(scp, []string{"payload", "bob@mac.pprbt:dest"}) {
		t.Fatalf("scp=%q error=%v", scp, err)
	}
	sftp, err := rewriteManagedToolArguments(managedToolTestContext(), "sftp", []string{"-o", "ConnectTimeout=5", "mac"}, resolver)
	if err != nil || !reflect.DeepEqual(sftp, []string{"-o", "ConnectTimeout=5", "adam@mac.pprbt"}) {
		t.Fatalf("sftp=%q error=%v", sftp, err)
	}
	rsync, err := rewriteManagedToolArguments(managedToolTestContext(), "rsync", []string{"-a", "bob@mac:src", "C:\\local\\dest"}, resolver)
	if err != nil || !reflect.DeepEqual(rsync, []string{"-a", "bob@mac.pprbt:src", "C:\\local\\dest"}) {
		t.Fatalf("rsync=%q error=%v", rsync, err)
	}
	wantCalls := []string{"mac@bob", "mac@", "mac@bob"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%q want=%q", calls, wantCalls)
	}
}

func TestRewriteManagedToolRejectsMissingRemoteOperand(t *testing.T) {
	var calls []string
	if _, err := rewriteManagedToolArguments(managedToolTestContext(), "scp", []string{"local-a", "local-b"}, managedToolTestResolver(&calls)); err == nil {
		t.Fatal("missing remote operand accepted")
	}
	if len(calls) != 0 {
		t.Fatalf("unexpected resolution: %q", calls)
	}
}
