package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

// Regression 2026-11-22: these commands disable flag parsing, so --help reached the tool path and failed generically.
func TestManagedSSHToolHelp(t *testing.T) {
	for _, tool := range []string{"scp", "rsync", "sftp"} {
		for _, args := range [][]string{{"--help"}, {"-h"}} {
			var out bytes.Buffer
			command := newManagedSSHToolCommand(tool)
			command.SetOut(&out)
			command.SetArgs(args)
			if err := command.Execute(); err != nil || !strings.Contains(out.String(), "standard "+tool+" arguments") {
				t.Fatalf("%s %q: out=%q err=%v", tool, args, out.String(), err)
			}
		}
	}
	for _, args := range [][]string{nil, {"-h", "-a", "mac:x", "."}, {"mac:x", "--help"}, {"--", "--help"}} {
		if managedToolHelp(args) {
			t.Fatalf("%q taken as a help request", args)
		}
	}
}

// Regression 2026-11-22: a refused operand printed only the generic failure. The reason is shown, the operand never is.
func TestManagedSSHOperandRefusalNamesReason(t *testing.T) {
	const hostile = "evil\x1b[31m"
	for _, test := range []struct{ tool, operand, want string }{
		{"scp", "-oProxyCommand=" + hostile + ":/tmp/x", "scp requires a remote [user@]machine:path operand"},
		{"rsync", hostile + ":/tmp/x", "rsync requires a remote [user@]machine:path operand"},
		{"sftp", hostile, managedssh.ErrMachineReferenceInvalid.Error()},
		{"sftp", "-o" + hostile, "sftp requires [user@]machine"},
		{"sftp", "$(" + hostile + ")@studio", managedssh.ErrSSHUsernameInvalid.Error()},
		{"sftp", "a@b@" + hostile, managedssh.ErrSSHAliasInvalid.Error()},
	} {
		var calls []string
		_, err := rewriteManagedToolArguments(managedToolTestContext(), test.tool, []string{test.operand}, managedToolTestDomain, managedToolTestResolver(&calls), nil)
		message := userFacingError(err)
		if err == nil || !strings.Contains(message, test.want) || strings.Contains(message, "evil") || strings.Contains(message, "\x1b") {
			t.Fatalf("%s %q: message=%q, want %q", test.tool, test.operand, message, test.want)
		}
	}
	_, _, err := parseSSHMachineOperand(hostile, nil)
	if message := userFacingError(machineOperandError(err)); !strings.Contains(message, managedssh.ErrMachineReferenceInvalid.Error()) {
		t.Fatalf("ssh: message=%q", message)
	}
}
