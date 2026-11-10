//go:build windows

package machineguard

import (
	"bytes"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
)

func TestMachineHostsLockedWindowsFilePreservesOriginal(t *testing.T) {
	path := testHostsFile(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	name, _ := windows.UTF16PtrFromString(path)
	// Reads and metadata checks can proceed, but atomic replacement is denied.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := replaceHostsFile(t.Context(), path, info, original, []byte("replacement")); err == nil {
		t.Fatal("locked file replaced")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("original damaged: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".paperboat-hosts-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("staging files leaked: %v %v", files, err)
	}
}

func TestSameFileSecurityIgnoresInheritedRulesAndOrderButNotExplicitOnes(t *testing.T) {
	parse := func(sddl string) *windows.SECURITY_DESCRIPTOR {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		return sd
	}
	base := parse("O:BAG:SYD:(A;ID;FA;;;SY)(A;ID;FA;;;BA)")
	for _, same := range []string{"O:BAG:SYD:AI(A;ID;FA;;;SY)", "O:BAG:SYD:AI(A;ID;FA;;;BA)(A;ID;FA;;;SY)(A;ID;0x1200a9;;;BU)"} {
		if !sameFileSecurity(base, parse(same)) {
			t.Fatalf("%s should match: only inherited rules differ", same)
		}
	}
	for _, other := range []string{"O:SYG:SYD:(A;ID;FA;;;SY)", "O:BAG:BAD:(A;ID;FA;;;SY)", "O:BAG:SYD:P(A;;FA;;;SY)", "O:BAG:SYD:(A;;FA;;;SY)(A;ID;FA;;;SY)", "O:BAG:SYD:(D;;FA;;;WD)(A;ID;FA;;;SY)"} {
		if sameFileSecurity(base, parse(other)) {
			t.Fatalf("%s must not match: owner, group, protection or an explicit rule differs", other)
		}
	}
	explicit, err := explicitACEs(func() *windows.ACL {
		dacl, _, _ := parse("O:BAG:SYD:(A;;FA;;;SY)(D;;FR;;;WD)(A;ID;FA;;;BA)").DACL()
		return dacl
	}())
	if err != nil || len(explicit) != 2 {
		t.Fatalf("explicit rules=%d err=%v; want the 2 non-inherited ones", len(explicit), err)
	}
}

func TestMachineHostsReplacementKeepsSecurityAndAddsNoExplicitRules(t *testing.T) {
	path := testHostsFile(t)
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, hostsSecurity)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if err := replaceHostsFile(t.Context(), path, info, original, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, hostsSecurity)
	if err != nil || !sameFileSecurity(before, after) {
		t.Fatalf("security changed by the replacement: %v\n before %s\n after  %s", err, before, after)
	}
	if leftover, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".paperboat-hosts-*")); len(leftover) != 0 {
		t.Fatalf("staging residue %v", leftover)
	}
}
