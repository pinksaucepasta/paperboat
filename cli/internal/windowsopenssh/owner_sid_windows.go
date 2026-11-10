//go:build windows

package windowsopenssh

import "golang.org/x/sys/windows"

func platformOwnerSID() string {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return ""
	}
	return user.User.Sid.String()
}

// Account lookup uses the local SAM's NetBIOS name. os.Hostname reports the
// physical DNS name, which can differ and must not turn a local user into a
// domain-qualified Match pattern.
func platformSSHComputerName() (string, error) {
	var name [16]uint16 // NetBIOS computer names have at most 15 characters.
	size := uint32(len(name))
	if err := windows.GetComputerNameEx(windows.ComputerNameNetBIOS, &name[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(name[:size]), nil
}
