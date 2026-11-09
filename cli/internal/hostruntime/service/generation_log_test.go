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
