//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pb function loads into the current PowerShell session, forwards every
// other call with its arguments, piped input and exit code, and never prints
// when pb is gone.
func TestPowerShellEnvLoadFunctionInTheRealShell(t *testing.T) {
	dir := t.TempDir()
	pb := filepath.Join(dir, "pb")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2 $3 $4\" = 'env hook --emit powershell' ]; then\n" +
		"  n=$(cat \"$FAKE_COUNTER\" 2>/dev/null || echo 0); n=$((n+1)); echo $n > \"$FAKE_COUNTER\"\n" +
		"  printf \"\\$env:PB_LOADED = '%s'\\n\" \"$n\"; printf '%s\\n' \"$FAKE_ASSIGNMENT\"; exit 0\n" +
		"fi\n[ \"$1\" = exit7 ] && exit 7\necho \"passthrough: $*\"\n[ \"$1\" = cat ] && cat\nexit 0\n"
	if err := os.WriteFile(pb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, "profile.ps1")
	if err := os.WriteFile(profile, []byte(envHookLine(envHookPowerShell, pb)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	show := "[Environment]::GetEnvironmentVariable"
	out := runPowerShell(t, ". '"+profile+"'; "+powerShellEchoUTF8+
		"'<'+"+show+"('PB_LOADED')+'>'; Show 'PB_HOSTILE'; pb env load; '<'+"+show+"('PB_LOADED')+'>'; Show 'PB_HOSTILE'; "+
		"pb status --x; pb env load extra; 'piped' | pb cat; pb exit7; 'exit=' + $LASTEXITCODE",
		"FAKE_COUNTER="+filepath.Join(dir, "count"), "FAKE_ASSIGNMENT="+envHookAssignment(envHookPowerShell, "PB_HOSTILE", powerShellHostile))
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(out), "\r\n", "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("output = %q", lines)
	}
	for _, i := range []int{1, 3} {
		if got := decodedPowerShellValue(t, lines[i]); got != powerShellHostile {
			t.Fatalf("line %d: hostile value = %q", i, got)
		}
	}
	want := map[int]string{0: "<1>", 2: "<2>", 4: "passthrough: status --x", 5: "passthrough: env load extra", 6: "passthrough: cat", 7: "piped", 8: "exit=7"}
	for i, text := range want {
		if lines[i] != text {
			t.Fatalf("line %d = %q, want %q (all: %q)", i, lines[i], text, lines)
		}
	}
}
