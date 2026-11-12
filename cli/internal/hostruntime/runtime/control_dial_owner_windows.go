//go:build windows

package runtime

import (
	"os"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// cacheFileTrusted: the remembered-address file is read only when this account (or the Administrators group a new
// file of an administrator is owned by, or SYSTEM) owns it; the state directory's inherited DACL keeps everyone
// else from writing there, and a link is already refused by the caller.
func cacheFileTrusted(path string, _ os.FileInfo) bool {
	token, err := windowssecurity.OpenEffectiveToken()
	if err != nil {
		return false
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	trusted := []*windows.SID{user.User.Sid}
	for _, well := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinLocalSystemSid} {
		if sid, err := windows.CreateWellKnownSid(well); err == nil {
			trusted = append(trusted, sid)
		}
	}
	for _, sid := range trusted {
		if owned, err := windowssecurity.CheckOwnerMatchesSID(path, sid); err == nil && owned {
			return true
		}
	}
	return false
}
