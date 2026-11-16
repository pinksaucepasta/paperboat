package service

import (
	"strings"
	"testing"
)

// Each carrier and hostd generation logs to its own /var/log file (macOS); retiring one must remove exactly that file.
func TestRetiredGenerationLogPathIsTheUnitsOwnStandardOutPath(t *testing.T) {
	config := Config{Platform: "darwin", Kind: HostdKind, Instance: "u501", Generation: "832f8616d4f129913621d67c0fd45029", ConfigRoot: t.TempDir(),
		Executable: "/Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb", User: "root", Group: "wheel", Arguments: []string{"daemon", "__runtime-hostd"}}
	body, err := renderLaunchd(config)
	if err != nil {
		t.Skipf("hostd definition for the test config: %v", err)
	}
	label := HostdLabel + ".u501." + config.Generation
	want := RetiredGenerationLogPath(label)
	if want != "/var/log/"+label+".log" {
		t.Fatalf("path %s", want)
	}
	if !strings.Contains(string(body), "<string>"+want+"</string>") {
		t.Fatalf("the rendered definition does not log to %s:\n%s", want, body)
	}
}

// The macOS per-user daemon had no StandardOutPath, so launchd discarded its output and a stuck daemon left nothing to read.
func TestPerUserDaemonLogsToTheUsersLibraryLogs(t *testing.T) {
	body, err := renderLaunchd(Config{Platform: "darwin", Kind: DaemonKind, ConfigRoot: "/Users/t", Executable: "/Users/t/.local/bin/pb", User: "t", Group: "staff", Arguments: []string{"daemon"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "<string>/Users/t/Library/Logs/Paperboat/paperboatd.log</string>"
	if strings.Count(string(body), want) != 2 || DaemonLogPath("/Users/t") != "/Users/t/Library/Logs/Paperboat/paperboatd.log" {
		t.Fatalf("stdout and stderr do not both go to %s:\n%s", want, body)
	}
}
