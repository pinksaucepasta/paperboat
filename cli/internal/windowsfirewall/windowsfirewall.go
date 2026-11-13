// Package windowsfirewall owns the inbound allow rules for the pinned pb.exe images of one installation.
// Without a rule, the first run of every new image pops the Windows Defender Firewall "allow access" dialog on
// the user's desktop (and leaves one stray rule per releases\versions\<version> path). Tailscale avoids the
// dialog by running as a service with its own WFP permit filters (ref/tailscale/wf/firewall.go); Paperboat's
// services run pinned pb.exe copies, so each copy gets a program rule in one group, pruned with the copies.
package windowsfirewall

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Runner runs one command, as service.ExecRunner does.
type Runner interface {
	Run(ctx context.Context, name string, arguments ...string) error
}

var errInvalid = errors.New("invalid Windows firewall rule input")

// Group is the rule group of one installation instance; uninstall removes the whole group.
func Group(instance string) string { return "Paperboat " + instance }

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func valid(group, program string) bool {
	return group != "" && !strings.ContainsAny(group+program, "\r\n\x00") &&
		len(program) > 3 && program[1] == ':' && program[2] == '\\' && strings.HasSuffix(strings.ToLower(program), ".exe")
}

// AllowScript allows inbound traffic to program and drops rules of the group whose program no longer exists
// or is program itself (so a reinstall over the same path leaves exactly one rule).
func AllowScript(group, program string) string {
	return strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"$g=" + quote(group),
		"$p=" + quote(program),
		"Get-NetFirewallRule -Group $g -ErrorAction SilentlyContinue|ForEach-Object{$a=($_|Get-NetFirewallApplicationFilter).Program;if(($a -ieq $p) -or -not (Test-Path -LiteralPath $a)){$_|Remove-NetFirewallRule}}",
		"New-NetFirewallRule -DisplayName 'Paperboat pb.exe' -Group $g -Direction Inbound -Action Allow -Profile Any -Program $p|Out-Null",
	}, ";")
}

// RemoveScript removes every rule of the group; a group without rules is not an error.
func RemoveScript(group string) string {
	return "$ErrorActionPreference='Stop';Get-NetFirewallRule -Group " + quote(group) + " -ErrorAction SilentlyContinue|Remove-NetFirewallRule"
}

func run(ctx context.Context, runner Runner, script string) error {
	timed, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return runner.Run(timed, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
}

// Allow adds the inbound allow rule for program (needs an elevated caller).
func Allow(ctx context.Context, runner Runner, group, program string) error {
	if !valid(group, program) {
		return errInvalid
	}
	if err := run(ctx, runner, AllowScript(group, program)); err != nil {
		return errors.Join(errors.New("add the Windows firewall rule for "+program), err)
	}
	return nil
}

// Remove deletes the group's rules.
func Remove(ctx context.Context, runner Runner, group string) error {
	if !valid(group, `C:\x.exe`) {
		return errInvalid
	}
	if err := run(ctx, runner, RemoveScript(group)); err != nil {
		return errors.Join(errors.New("remove the Windows firewall rules of "+group), err)
	}
	return nil
}
