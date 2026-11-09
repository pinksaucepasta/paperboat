//go:build windows

package main

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

const envHookConsole = "CONOUT$"

// envHookDocumentsDir is the Documents folder PowerShell keeps its profiles
// in, wherever the user redirected it (OneDrive); home is the fallback.
func envHookDocumentsDir(home string) string {
	if dir, err := windows.KnownFolderPath(windows.FOLDERID_Documents, windows.KF_FLAG_DEFAULT); err == nil {
		return dir
	}
	return filepath.Join(home, "Documents")
}
