//go:build windows

package machineguard

import (
	"golang.org/x/sys/windows"
	"testing"
)

func TestBrowserDomainCallerRequiresCurrentSID(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner := user.User.Sid.String()
	if err := validateBrowserDomainCaller(owner); err != nil {
		t.Fatal(err)
	}
	other := "S-1-5-21-1-2-3-1000"
	if other == owner {
		other = "S-1-5-21-1-2-3-1001"
	}
	for _, bad := range []string{other, "1000", "foreign", "s-1-5-18"} {
		if validateBrowserDomainCaller(bad) == nil {
			t.Fatalf("accepted another or noncanonical SID %q", bad)
		}
	}
}
