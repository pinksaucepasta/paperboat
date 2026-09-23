//go:build windows

package deviceguard

import "golang.org/x/sys/windows"

func canReconfigureRange(conn controlConn, _ string) bool {
	peer, ok := conn.(*windowsControlConn)
	if !ok || peer.pid == 0 {
		return false
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, peer.pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err = windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	member, err := token.IsMember(admins)
	return err == nil && member
}
