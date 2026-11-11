//go:build windows

package windowssecurity

import (
	"errors"

	"golang.org/x/sys/windows"
)

// OpenEffectiveToken opens the token the calling thread acts with: its impersonation token when it has one
// (WithUnelevatedToken, a named-pipe client), the process token otherwise. Identity and owner decisions use it,
// not OpenCurrentProcessToken, so they follow the thread's own identity (Tailscale util/winutil). The caller closes it.
// Not for a listener or privilege gate that decides what the SERVICE is (ListenWorkloads, machineguard requirePrivilege):
// those keep the process token, which an impersonating thread must not change.
func OpenEffectiveToken() (windows.Token, error) {
	var token windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, windows.ERROR_NO_TOKEN) {
		return 0, err
	}
	return windows.OpenCurrentProcessToken()
}
