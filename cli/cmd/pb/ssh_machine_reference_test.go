package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

const referenceTestMachineID = "machine_0247b287-39ae-4033-b870-1893b378d0c9"

// Regression FIXTURES 2026-11-22: pb scp and pb rsync passed a machine_<id> operand to OpenSSH unresolved while
// pb ssh and pb sftp resolved it. Every managed SSH command accepts the same machine references: the machine
// name, its namespaced alias, and the stable machine ID.
func TestManagedSSHCommandsAcceptSameMachineReferences(t *testing.T) {
	domain := func() string { return managedToolTestDomain }
	for _, test := range []struct{ name, operand, target string }{
		{"name", "studio", "studio"},
		{"name in another case", "Studio", "studio"},
		{"namespaced alias", "studio." + managedToolTestDomain, "studio"},
		{"machine id", referenceTestMachineID, referenceTestMachineID},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, user, err := parseSSHMachineOperand("bob@"+test.operand, domain)
			if err != nil || target != test.target || user != "bob" {
				t.Fatalf("ssh: target=%q user=%q err=%v, want %q", target, user, err, test.target)
			}
			host := "adam@" + test.target + "." + managedToolTestDomain
			for _, tool := range []struct {
				name      string
				arguments []string
				want      []string
			}{
				{"scp", []string{"-r", "./local", test.operand + ":/tmp/x"}, []string{"-r", "./local", host + ":/tmp/x"}},
				{"rsync", []string{"-a", test.operand + ":src/", "./dest"}, []string{"-a", host + ":src/", "./dest"}},
				{"sftp", []string{"-o", "ConnectTimeout=5", test.operand}, []string{"-o", "ConnectTimeout=5", host}},
			} {
				var calls []string
				got, err := rewriteManagedToolArguments(managedToolTestContext(), tool.name, tool.arguments, managedToolTestDomain, managedToolTestResolver(&calls), nil)
				if err != nil || !reflect.DeepEqual(got, tool.want) || !reflect.DeepEqual(calls, []string{test.target + "@"}) {
					t.Fatalf("%s: rewrite=%q calls=%q err=%v, want %q", tool.name, got, calls, err, tool.want)
				}
			}
		})
	}
}

func TestManagedSSHCommandsRefuseNonMachineReferences(t *testing.T) {
	domain := func() string { return managedToolTestDomain }
	for _, operand := range []string{
		"-oProxyCommand=touch /tmp/pwned",
		"--",
		"machine_",
		"machine_-x",
		"machine_a;b",
		"machine_a b",
		"machine_$(id)",
		"machine_a/../b",
		"other_name",
		"-studio",
		"studio.other.example",
		"a@b@studio",
		"bad\nname",
	} {
		t.Run(operand, func(t *testing.T) {
			if target, _, err := parseSSHMachineOperand(operand, domain); err == nil {
				t.Fatalf("ssh accepted %q as %q", operand, target)
			}
			var calls []string
			// sftp refuses the operand; scp and rsync leave it to the tool unresolved and, with no machine operand
			// left, refuse the command.
			if got, err := rewriteManagedToolArguments(managedToolTestContext(), "sftp", []string{"--", operand}, managedToolTestDomain, managedToolTestResolver(&calls), nil); err == nil {
				t.Fatalf("sftp accepted %q: %q", operand, got)
			}
			for _, tool := range []string{"scp", "rsync"} {
				if got, err := rewriteManagedToolArguments(managedToolTestContext(), tool, []string{"./local", operand + ":/tmp/x"}, managedToolTestDomain, managedToolTestResolver(&calls), nil); err == nil {
					t.Fatalf("%s accepted %q: %q", tool, operand, got)
				}
			}
			if len(calls) != 0 {
				t.Fatalf("resolved %q: %q", operand, calls)
			}
		})
	}
	if _, _, err := parseSSHMachineOperand("other_name", domain); !errors.Is(err, managedssh.ErrMachineReferenceInvalid) {
		t.Fatalf("err=%v, want ErrMachineReferenceInvalid", err)
	}
}

// Alias creation stays strict: a machine ID shape is a valid reference but never a valid alias.
func TestMachineIDIsNotAValidAlias(t *testing.T) {
	if managedssh.ValidateAlias(referenceTestMachineID) == nil {
		t.Fatal("machine ID accepted as an alias")
	}
	if !managedssh.ValidMachineReference(referenceTestMachineID) || !managedssh.ValidMachineReference("studio") {
		t.Fatal("machine reference refused")
	}
}
