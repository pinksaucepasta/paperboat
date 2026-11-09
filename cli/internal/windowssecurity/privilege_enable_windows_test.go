//go:build windows

package windowssecurity

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// A privilege the token does not hold is an error, not a silent no-op (AdjustTokenPrivileges reports success with
// ERROR_NOT_ALL_ASSIGNED).
func TestEnablePrivilegeFailsForAPrivilegeTheTokenDoesNotHold(t *testing.T) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	// SeTcbPrivilege (act as part of the operating system) is held by LocalSystem only.
	err := enablePrivilege(token, "SeTcbPrivilege")
	if err == nil {
		t.Skip("this token holds SeTcbPrivilege (running as LocalSystem)")
	}
	if !errors.Is(err, windows.ERROR_NOT_ALL_ASSIGNED) {
		t.Fatalf("a privilege that is not held gave %v, want ERROR_NOT_ALL_ASSIGNED", err)
	}
}
