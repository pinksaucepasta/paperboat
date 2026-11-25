package processlaunch

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// SystemTool returns the full path of a bare Windows tool name an elevated process starts: PATH, WINDIR and SystemRoot
// can be set by the user who elevates. Other names are returned as they are; a failed lookup returns "", which cannot run.
func SystemTool(name string) string {
	directory := ""
	switch name {
	case "powershell.exe":
		directory = `WindowsPowerShell\v1.0`
	case "schtasks.exe":
	default:
		return name
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	return filepath.Join(system, directory, name)
}
