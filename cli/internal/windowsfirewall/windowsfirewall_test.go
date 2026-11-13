package windowsfirewall

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorder struct {
	commands [][]string
	err      error
}

func (r *recorder) Run(_ context.Context, name string, arguments ...string) error {
	r.commands = append(r.commands, append([]string{name}, arguments...))
	return r.err
}

func TestAllowBuildsOneProgramRuleAndQuotesTheValues(t *testing.T) {
	r := &recorder{}
	program := `C:\Program Files\Paperboat\users\o'brien\releases\versions\1\pb.exe`
	if err := Allow(t.Context(), r, Group("o'brien"), program); err != nil {
		t.Fatal(err)
	}
	if len(r.commands) != 1 || r.commands[0][0] != "powershell.exe" {
		t.Fatalf("commands = %v", r.commands)
	}
	script := r.commands[0][len(r.commands[0])-1]
	for _, want := range []string{
		`$g='Paperboat o''brien'`, `$p='C:\Program Files\Paperboat\users\o''brien\releases\versions\1\pb.exe'`,
		"New-NetFirewallRule", "-Direction Inbound -Action Allow", "-Program $p", "Test-Path -LiteralPath $a", "Remove-NetFirewallRule",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
	}
}

func TestRemoveDropsTheWholeGroup(t *testing.T) {
	r := &recorder{}
	if err := Remove(t.Context(), r, Group("u1")); err != nil {
		t.Fatal(err)
	}
	script := r.commands[0][len(r.commands[0])-1]
	if !strings.Contains(script, "Get-NetFirewallRule -Group 'Paperboat u1'") || !strings.HasSuffix(script, "Remove-NetFirewallRule") {
		t.Fatalf("script = %s", script)
	}
}

func TestInvalidInputIsRefusedAndFailuresNameTheStep(t *testing.T) {
	r := &recorder{err: errors.New("boom")}
	for _, program := range []string{"", `relative\pb.exe`, `C:\pb.dll`, "C:\\a\nb.exe"} {
		if err := Allow(t.Context(), r, "g", program); !errors.Is(err, errInvalid) {
			t.Fatalf("program %q: %v", program, err)
		}
	}
	if len(r.commands) != 0 {
		t.Fatalf("ran %v", r.commands)
	}
	err := Allow(t.Context(), r, "g", `C:\pb.exe`)
	if err == nil || !strings.Contains(err.Error(), `add the Windows firewall rule for C:\pb.exe`) || !errors.Is(err, r.err) {
		t.Fatalf("err = %v", err)
	}
}
