//go:build windows

package windowssecurity

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	createRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

	// ErrStillElevated is a filtered token that kept its administrator rights; nothing ran with it.
	ErrStillElevated = errors.New("the unelevated token still holds administrator rights")
)

// WithUnelevatedToken runs operation on a pinned thread that impersonates the process's own user without its
// elevation: the token UAC gives a standard session (Administrators deny-only, privileges dropped, medium
// integrity). An elevated process uses it for everything it does in places its user can write: a link the user
// plants there then reaches only what the user could reach anyway, however the race goes. It fails closed, the
// caller must not be impersonating already, and a thread that cannot be reverted never returns to Go's pool.
func WithUnelevatedToken(operation func() error) error {
	if operation == nil {
		return windows.ERROR_INVALID_PARAMETER
	}
	var process, limited, thread windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &process); err != nil {
		return err
	}
	defer process.Close()
	const disableMaxPrivilege, luaToken = 0x1, 0x4
	if created, _, err := createRestrictedToken.Call(uintptr(process), disableMaxPrivilege|luaToken, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&limited))); created == 0 {
		return err
	}
	defer limited.Close()
	if err := windows.DuplicateTokenEx(limited, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE|windows.TOKEN_ADJUST_DEFAULT, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &thread); err != nil {
		return err
	}
	defer thread.Close()
	medium, err := windows.CreateWellKnownSid(windows.WinMediumLabelSid)
	if err != nil {
		return err
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: medium, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(thread, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size()); err != nil {
		return err
	}
	runtime.KeepAlive(medium)
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	if member, err := thread.IsMember(administrators); err != nil {
		return err
	} else if member {
		return ErrStillElevated
	}

	runtime.LockOSThread()
	var existing windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &existing); !errors.Is(err, windows.ERROR_NO_TOKEN) {
		if err == nil {
			err = errors.Join(windows.ERROR_INVALID_PARAMETER, existing.Close())
		}
		runtime.UnlockOSThread()
		return err
	}
	if err := windows.SetThreadToken(nil, thread); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			terminateTokenLeak(err)
			return
		}
		runtime.UnlockOSThread()
	}()
	return operation()
}
