package managedssh

import "fmt"

// UnsafeSSHPathError: the user's .ssh folder has an owner or a write permission Windows OpenSSH would not trust,
// so Paperboat does not write its managed SSH files there.
type UnsafeSSHPathError struct {
	Path    string
	Problem string
}

func (e *UnsafeSSHPathError) Error() string {
	return fmt.Sprintf("Managed SSH cannot use %s: %s. In PowerShell run: icacls \"%s\" /setowner \"$env:USERNAME\"; "+
		"then remove write access for every other user except SYSTEM and Administrators (icacls \"%s\" /remove <user>). "+
		"Paperboat retries within a minute; check with pb ssh doctor <machine>.", e.Path, e.Problem, e.Path, e.Path)
}

func (e *UnsafeSSHPathError) Unwrap() error { return ErrOpenSSHConfigConflict }
