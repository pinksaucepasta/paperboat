//go:build windows

package windowsopenssh

import (
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

var createProtectedDirectory = service.CreateProtectedWindowsDirectory

// msiDirectory is in Program Files: unlike ProgramData, only administrators can create anything there, so nobody can
// plant it or a parent of it and swap the verified file. The environment is not asked: a user can set it.
var msiDirectory = func() (string, error) {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	return filepath.Join(programFiles, "Paperboat OpenSSH installer"), err
}

// msiexecPath never trusts the environment: a user can set SystemRoot for the process they elevate.
func msiexecPath() (string, error) {
	directory, err := windows.GetSystemDirectory()
	return filepath.Join(directory, "msiexec.exe"), err
}
