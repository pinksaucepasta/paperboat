package processlaunch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSystemToolIgnoresThePlantedEnvironment(t *testing.T) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PATH", "WINDIR", "SystemRoot"} {
		t.Setenv(name, t.TempDir())
	}
	for _, name := range []string{"powershell.exe", "schtasks.exe"} {
		path := SystemTool(name)
		if _, err := os.Stat(path); err != nil || !strings.HasPrefix(path, system+`\`) || filepath.Base(path) != name {
			t.Errorf("SystemTool(%s) = %s, %v", name, path, err)
		}
	}
	if got := SystemTool("winget.exe"); got != "winget.exe" {
		t.Errorf("other tool = %s", got)
	}
}
