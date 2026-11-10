//go:build windows

package machineguard

import (
	"fmt"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

const aceInheritanceFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE | windows.NO_PROPAGATE_INHERIT_ACE | windows.INHERIT_ONLY_ACE

// explicitACEs returns the allow and deny ACEs of dacl that are not inherited, as access entries that rebuild them.
// Any other ACE type is refused: replacing the hosts file must not silently drop a rule it cannot reproduce.
func explicitACEs(dacl *windows.ACL) ([]windows.EXPLICIT_ACCESS, error) {
	if dacl == nil {
		return nil, nil
	}
	var entries []windows.EXPLICIT_ACCESS
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		mode := windows.ACCESS_MODE(windows.GRANT_ACCESS)
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		case windows.ACCESS_DENIED_ACE_TYPE:
			mode = windows.DENY_ACCESS
		default:
			return nil, fmt.Errorf("hosts file has an access rule of type %d that cannot be reproduced; left unchanged", ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.ACCESS_MASK(ace.Mask), AccessMode: mode,
			Inheritance: uint32(ace.Header.AceFlags & aceInheritanceFlags),
			Trustee:     windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN, TrusteeValue: windows.TrusteeValueFromSID(sid)}})
	}
	return entries, nil
}

// fileSecurityKey is what must survive a hosts replacement: owner, group, whether inheritance is blocked, and the
// explicit access rules. Inherited rules and the auto-inherited bit follow from the parent directory.
func fileSecurityKey(sd *windows.SECURITY_DESCRIPTOR) ([]string, error) {
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	group, _, err := sd.Group()
	if err != nil {
		return nil, err
	}
	control, _, err := sd.Control()
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	key := []string{"owner " + owner.String(), "group " + group.String(), fmt.Sprintf("protected %t", control&windows.SE_DACL_PROTECTED != 0)}
	var rules []string
	for index := uint32(0); dacl != nil && index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			rules = append(rules, fmt.Sprintf("ace %d %#x %#x %s", ace.Header.AceType, ace.Header.AceFlags&aceInheritanceFlags, ace.Mask, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()))
		}
	}
	sort.Strings(rules)
	return append(key, rules...), nil
}

func sameFileSecurity(a, b *windows.SECURITY_DESCRIPTOR) bool {
	first, err := fileSecurityKey(a)
	if err != nil {
		return false
	}
	second, err := fileSecurityKey(b)
	if err != nil || len(first) != len(second) {
		return false
	}
	for i := range first {
		if first[i] != second[i] {
			return false
		}
	}
	return true
}
