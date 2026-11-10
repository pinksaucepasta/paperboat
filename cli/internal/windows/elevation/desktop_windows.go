//go:build windows

package elevation

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	userObjectFlags = 1 // UOI_FLAGS
	stationVisible  = 1 // WSF_VISIBLE
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procGetProcessWindowStation  = user32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformation = user32.NewProc("GetUserObjectInformationW")
)

// ConsentPromptAvailable reports whether this process runs on the interactive window station, the only place a UAC
// prompt can appear. A batch or service logon (a scheduled task, session 0) has a hidden station: ShellExecuteEx
// "runas" there never returns. An unreadable station counts as available, so a real prompt is never refused by mistake.
func ConsentPromptAvailable() bool {
	station, _, _ := procGetProcessWindowStation.Call()
	if station == 0 {
		return true
	}
	var flags struct {
		Inherit  int32
		Reserved int32
		Flags    uint32
	}
	var needed uint32
	ok, _, _ := procGetUserObjectInformation.Call(station, userObjectFlags, uintptr(unsafe.Pointer(&flags)),
		unsafe.Sizeof(flags), uintptr(unsafe.Pointer(&needed)))
	return ok == 0 || flags.Flags&stationVisible != 0
}
