package machineguard

import (
	"strings"
	"testing"
)

// The guard's launchd job logs to /var/log/<label>.log like the other Paperboat jobs; without it macOS threw
// away the guard's output (its gateway refusal lines), which journald keeps on Linux.
func TestDarwinGuardLaunchPlistLogsLikeTheOtherJobs(t *testing.T) {
	unit, err := darwinGuardLaunchPlist("io.paperboat.machineguard", []string{"/Library/PrivilegedHelperTools/io.paperboat.machineguard", "daemon", "machine-guard", "run"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>StandardOutPath</key><string>/var/log/io.paperboat.machineguard.log</string>",
		"<key>StandardErrorPath</key><string>/var/log/io.paperboat.machineguard.log</string>",
		"<string>machine-guard</string>", "<key>Label</key><string>io.paperboat.machineguard</string>",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("guard plist lacks %q:\n%s", want, unit)
		}
	}
}
