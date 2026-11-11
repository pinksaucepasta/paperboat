//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestOwnerOnlyDescriptorAcceptsOnlyThisUserSystemAndAdministrators(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := user.User.Sid.String()
	for _, test := range []struct {
		sddl   string
		accept bool
	}{
		{"O:" + me + "D:P(A;;FA;;;" + me + ")", true},
		{"O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + me + ")", true},
		{"O:" + me + "D:P(A;;FA;;;" + me + ")(A;;FR;;;WD)", false},
		{"O:" + me + "D:P(A;;FA;;;" + me + ")(A;;FR;;;BU)", false},
		{"O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;" + me + ")", false},
		{"O:" + me + "D:(A;;FA;;;" + me + ")", false},
		{"O:" + me + "D:NO_ACCESS_CONTROL", false},
	} {
		descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
		if err != nil {
			t.Fatal(err)
		}
		if got := checkOwnerOnlyDescriptor(descriptor) == nil; got != test.accept {
			t.Errorf("%s: accepted=%v, want %v", test.sddl, got, test.accept)
		}
	}
}
