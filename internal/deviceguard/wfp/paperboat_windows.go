//go:build windows

// Package wfp adapts the MIT-licensed WireGuard for Windows WFP ABI bindings
// in this directory to Paperboat's persistent loopback guard.
package wfp

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type Lease struct{ IP, SID string }
type wfpObjectInstaller func(uintptr) error

var sublayerKey = windows.GUID{Data1: 0x70617065, Data2: 0x7262, Data3: 0x4f61, Data4: [8]byte{0xb4, 0x2d, 0x77, 0x66, 0x70, 0x2d, 0x30, 0x32}}
var legacySublayerKey = windows.GUID{Data1: 0x70617065, Data2: 0x7262, Data3: 0x6f61, Data4: [8]byte{0x74, 0x2d, 0x77, 0x66, 0x70, 0x2d, 0x30, 0x32}}

const alreadyExists windows.Errno = 0x80320009

func Apply(leases, known []Lease, configuration ...string) error {
	loopbackCIDR, dnsEndpoint := "127.100.0.0/16", "127.100.0.1:53"
	if len(configuration) > 0 && configuration[0] != "" {
		loopbackCIDR = configuration[0]
	}
	if len(configuration) > 1 && configuration[1] != "" {
		dnsEndpoint = configuration[1]
	}
	parsedPrefix, parseErr := netip.ParsePrefix(loopbackCIDR)
	protectedPrefixes := []netip.Prefix{}
	protectedValues := []string{loopbackCIDR}
	if len(configuration) > 2 {
		protectedValues = append(protectedValues, configuration[2:]...)
	}
	seenProtected := make(map[netip.Prefix]bool)
	for _, value := range protectedValues {
		prefix, prefixErr := netip.ParsePrefix(value)
		if prefixErr != nil || !prefix.Addr().Is4() || prefix.Bits() != 16 || prefix != prefix.Masked() {
			return errors.New("invalid Paperboat WFP protected loopback range")
		}
		if !seenProtected[prefix] {
			seenProtected[prefix] = true
			protectedPrefixes = append(protectedPrefixes, prefix)
		}
	}
	dnsHost, _, splitErr := net.SplitHostPort(dnsEndpoint)
	parsedDNS, dnsErr := netip.ParseAddr(dnsHost)
	if parseErr != nil || splitErr != nil || dnsErr != nil || !parsedPrefix.Addr().Is4() || parsedPrefix.Bits() != 16 || !parsedDNS.Is4() || !parsedPrefix.Contains(parsedDNS) {
		return errors.New("invalid Paperboat WFP loopback configuration")
	}
	var engine uintptr
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, nil, unsafe.Pointer(&engine)); err != nil {
		return err
	}
	defer fwpmEngineClose0(engine)
	return runTransaction(engine, func(engine uintptr) error {
		baseNames := []string{"paperboat-dns-permit-tcp", "paperboat-dns-permit-udp", "paperboat-in-permit", "paperboat-in-block", "paperboat-out-block"}
		for _, protectedPrefix := range protectedPrefixes {
			second := int(protectedPrefix.Addr().As4()[1])
			baseNames = append(baseNames, fmt.Sprintf("paperboat-in-block-%d", second), fmt.Sprintf("paperboat-out-block-%d", second))
		}
		if err := deleteRuleKeys(engine, baseNames, known); err != nil {
			return err
		}
		result, _, _ := sublayerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&legacySublayerKey)))
		if result != 0 && !notFound(result) {
			return windows.Errno(result)
		}
		if err := ensureBase(engine); err != nil {
			return fmt.Errorf("base objects: %w", err)
		}
		app, err := getCurrentProcessAppID()
		if err != nil {
			return err
		}
		defer fwpmFreeMemory0(unsafe.Pointer(&app))
		system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
		systemSD, err := sidDescriptor(system)
		if err != nil {
			return err
		}
		systemBlob := wtFwpByteBlob{size: systemSD.Length(), data: (*byte)(unsafe.Pointer(systemSD))}
		prefixBytes := parsedPrefix.Addr().As4()
		dnsBytes := parsedDNS.As4()
		prefix := wtFwpV4AddrAndMask{addr: binary.BigEndian.Uint32(prefixBytes[:]), mask: 0xffff0000}
		dnsAddress := binary.BigEndian.Uint32(dnsBytes[:])
		for _, transport := range []struct {
			name     string
			protocol uint8
		}{{"tcp", uint8(cIPPROTO_TCP)}, {"udp", uint8(cIPPROTO_UDP)}} {
			if err = addRule(engine, "paperboat-dns-permit-"+transport.name, cFWPM_LAYER_ALE_AUTH_CONNECT_V4, cFWP_ACTION_PERMIT, 15, []wtFwpmFilterCondition0{uint8Condition(cFWPM_CONDITION_IP_PROTOCOL, transport.protocol), uint32Condition(cFWPM_CONDITION_IP_REMOTE_ADDRESS, dnsAddress), uint16Condition(cFWPM_CONDITION_IP_REMOTE_PORT, 53)}); err != nil {
				return fmt.Errorf("DNS %s permit: %w", transport.name, err)
			}
		}
		if err = addRule(engine, "paperboat-in-permit", cFWPM_LAYER_ALE_AUTH_RECV_ACCEPT_V4, cFWP_ACTION_PERMIT, 14, []wtFwpmFilterCondition0{protocol(), addrCondition(cFWPM_CONDITION_IP_LOCAL_ADDRESS, &prefix), blobCondition(cFWPM_CONDITION_ALE_APP_ID, cFWP_BYTE_BLOB_TYPE, unsafe.Pointer(app)), blobCondition(cFWPM_CONDITION_ALE_USER_ID, cFWP_SECURITY_DESCRIPTOR_TYPE, unsafe.Pointer(&systemBlob))}); err != nil {
			return fmt.Errorf("inbound app permit: %w", err)
		}
		runtime.KeepAlive(systemSD)
		runtime.KeepAlive(systemBlob)
		for _, blockedPrefix := range protectedPrefixes {
			bytes := blockedPrefix.Addr().As4()
			blocked := wtFwpV4AddrAndMask{addr: binary.BigEndian.Uint32(bytes[:]), mask: 0xffff0000}
			second := int(bytes[1])
			if err = addRule(engine, fmt.Sprintf("paperboat-in-block-%d", second), cFWPM_LAYER_ALE_AUTH_RECV_ACCEPT_V4, cFWP_ACTION_BLOCK, 12, []wtFwpmFilterCondition0{protocol(), addrCondition(cFWPM_CONDITION_IP_LOCAL_ADDRESS, &blocked)}); err != nil {
				return fmt.Errorf("inbound block: %w", err)
			}
			if err = addRule(engine, fmt.Sprintf("paperboat-out-block-%d", second), cFWPM_LAYER_ALE_AUTH_CONNECT_V4, cFWP_ACTION_BLOCK, 12, []wtFwpmFilterCondition0{protocol(), addrCondition(cFWPM_CONDITION_IP_REMOTE_ADDRESS, &blocked)}); err != nil {
				return fmt.Errorf("outbound block: %w", err)
			}
			runtime.KeepAlive(blocked)
		}
		seen := map[string]bool{}
		for _, lease := range leases {
			key := lease.IP + "\x00" + lease.SID
			if seen[key] {
				continue
			}
			seen[key] = true
			ip, parseErr := netip.ParseAddr(lease.IP)
			sid, sidErr := windows.StringToSid(lease.SID)
			if parseErr != nil || !ip.Is4() || sidErr != nil {
				return errors.New("invalid Paperboat WFP lease")
			}
			sd, sdErr := sidDescriptor(sid)
			if sdErr != nil {
				return sdErr
			}
			address := binary.BigEndian.Uint32(ip.AsSlice())
			sdBlob := wtFwpByteBlob{size: sd.Length(), data: (*byte)(unsafe.Pointer(sd))}
			conditions := []wtFwpmFilterCondition0{protocol(), uint32Condition(cFWPM_CONDITION_IP_REMOTE_ADDRESS, address), blobCondition(cFWPM_CONDITION_ALE_USER_ID, cFWP_SECURITY_DESCRIPTOR_TYPE, unsafe.Pointer(&sdBlob))}
			if err = addRule(engine, "paperboat-user-"+lease.IP, cFWPM_LAYER_ALE_AUTH_CONNECT_V4, cFWP_ACTION_PERMIT, 15, conditions, key); err != nil {
				return err
			}
			runtime.KeepAlive(sd)
			runtime.KeepAlive(sdBlob)
		}
		runtime.KeepAlive(prefix)
		runtime.KeepAlive(app)
		return nil
	})
}

func deleteRuleKeys(engine uintptr, names []string, leases []Lease) error {
	keys := make([]windows.GUID, 0, len(names)*2+len(leases)*2)
	for _, name := range names {
		keys = append(keys, guidFor(name), legacyGUIDFor(name))
	}
	for _, lease := range leases {
		material := lease.IP + "\x00" + lease.SID
		keys = append(keys, guidFor(material), legacyGUIDFor("paperboat-user-"+material))
	}
	for i := range keys {
		result, _, _ := filterDeleteProc.Call(engine, uintptr(unsafe.Pointer(&keys[i])))
		if result != 0 && !notFound(result) {
			return windows.Errno(result)
		}
	}
	return nil
}
func ensureBase(engine uintptr) error {
	name, _ := createWtFwpmDisplayData0("Paperboat device guard", "Persistent protected device-name policy")
	sub := wtFwpmSublayer0{subLayerKey: sublayerKey, displayData: *name, flags: cFWPM_SUBLAYER_FLAG_PERSISTENT, weight: 0xffff}
	if err := fwpmSubLayerAdd0(engine, &sub, 0); err != nil && !isCode(err, uint32(alreadyExists)) {
		return fmt.Errorf("sublayer: %w (%T %#v)", err, err, err)
	}
	return nil
}
func addRule(engine uintptr, name string, layer windows.GUID, action wtFwpActionType, weight uint8, conditions []wtFwpmFilterCondition0, keyMaterial ...string) error {
	trace("add " + name)
	display, err := createWtFwpmDisplayData0(name, "")
	if err != nil {
		return err
	}
	keyValue := name
	if len(keyMaterial) > 0 {
		keyValue = keyMaterial[0]
	}
	key := guidFor(keyValue)
	filter := wtFwpmFilter0{filterKey: key, displayData: *display, flags: cFWPM_FILTER_FLAG_PERSISTENT | cFWPM_FILTER_FLAG_CLEAR_ACTION_RIGHT, layerKey: layer, subLayerKey: sublayerKey, weight: filterWeight(weight), numFilterConditions: uint32(len(conditions)), action: wtFwpmAction0{_type: action}}
	if len(conditions) > 0 {
		filter.filterCondition = &conditions[0]
	}
	var id uint64
	err = fwpmFilterAdd0(engine, &filter, 0, &id)
	trace("added " + name)
	if isCode(err, uint32(alreadyExists)) {
		return nil
	}
	return err
}
func guidFor(value string) windows.GUID {
	sum := sha256.Sum256([]byte(value))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	return windows.GUID{Data1: binary.LittleEndian.Uint32(sum[0:4]), Data2: binary.LittleEndian.Uint16(sum[4:6]), Data3: binary.LittleEndian.Uint16(sum[6:8]), Data4: [8]byte(sum[8:16])}
}
func legacyGUIDFor(value string) windows.GUID {
	sum := sha256.Sum256([]byte(value))
	return windows.GUID{Data1: binary.LittleEndian.Uint32(sum[0:4]), Data2: binary.LittleEndian.Uint16(sum[4:6]), Data3: binary.LittleEndian.Uint16(sum[6:8]), Data4: [8]byte(sum[8:16])}
}
func protocol() wtFwpmFilterCondition0 {
	return uint8Condition(cFWPM_CONDITION_IP_PROTOCOL, uint8(cIPPROTO_TCP))
}
func uint8Condition(key windows.GUID, v uint8) wtFwpmFilterCondition0 {
	return wtFwpmFilterCondition0{fieldKey: key, matchType: cFWP_MATCH_EQUAL, conditionValue: wtFwpConditionValue0{_type: cFWP_UINT8, value: uintptr(v)}}
}
func uint32Condition(key windows.GUID, v uint32) wtFwpmFilterCondition0 {
	return wtFwpmFilterCondition0{fieldKey: key, matchType: cFWP_MATCH_EQUAL, conditionValue: wtFwpConditionValue0{_type: cFWP_UINT32, value: uintptr(v)}}
}
func uint16Condition(key windows.GUID, v uint16) wtFwpmFilterCondition0 {
	return wtFwpmFilterCondition0{fieldKey: key, matchType: cFWP_MATCH_EQUAL, conditionValue: wtFwpConditionValue0{_type: cFWP_UINT16, value: uintptr(v)}}
}
func addrCondition(key windows.GUID, v *wtFwpV4AddrAndMask) wtFwpmFilterCondition0 {
	return wtFwpmFilterCondition0{fieldKey: key, matchType: cFWP_MATCH_EQUAL, conditionValue: wtFwpConditionValue0{_type: cFWP_V4_ADDR_MASK, value: uintptr(unsafe.Pointer(v))}}
}
func blobCondition(key windows.GUID, kind wtFwpDataType, v unsafe.Pointer) wtFwpmFilterCondition0 {
	return wtFwpmFilterCondition0{fieldKey: key, matchType: cFWP_MATCH_EQUAL, conditionValue: wtFwpConditionValue0{_type: kind, value: uintptr(v)}}
}
func sidDescriptor(sid *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{AccessPermissions: cFWP_ACTRL_MATCH_FILTER, AccessMode: windows.GRANT_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(sid)}}}, nil)
	if err != nil {
		return nil, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err = sd.SetDACL(acl, true, false); err != nil {
		return nil, err
	}
	return sd.ToSelfRelative()
}

var (
	filterDeleteProc   = windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmFilterDeleteByKey0")
	sublayerDeleteProc = windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmSubLayerDeleteByKey0")
)

func Remove(leases []Lease) error {
	var engine uintptr
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, nil, unsafe.Pointer(&engine)); err != nil {
		return err
	}
	defer fwpmEngineClose0(engine)
	return runTransaction(engine, func(engine uintptr) error {
		names := ownedFilterNames()
		for _, lease := range leases {
			names = append(names, lease.IP+"\x00"+lease.SID)
		}
		for _, name := range names {
			for _, key := range []windows.GUID{guidFor(name), legacyGUIDFor(name)} {
				result, _, _ := filterDeleteProc.Call(engine, uintptr(unsafe.Pointer(&key)))
				if result != 0 && !notFound(result) {
					return windows.Errno(result)
				}
			}
		}
		for _, key := range []windows.GUID{sublayerKey, legacySublayerKey} {
			result, _, _ := sublayerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&key)))
			if result != 0 && !notFound(result) {
				return windows.Errno(result)
			}
		}
		return nil
	})
}

// ownedFilterNames returns every fixed Paperboat base/range filter key. Range
// changes can retain any permitted 127.N/16, so uninstall must remove the
// bounded key space rather than only the currently active prefix. GUIDs remain
// derived from these exact Paperboat-owned names; no foreign provider or filter
// is enumerated or deleted.
func ownedFilterNames() []string {
	names := []string{"paperboat-dns-permit-tcp", "paperboat-dns-permit-udp", "paperboat-in-permit", "paperboat-in-block", "paperboat-out-block"}
	for second := 1; second <= 254; second++ {
		names = append(names, fmt.Sprintf("paperboat-in-block-%d", second), fmt.Sprintf("paperboat-out-block-%d", second))
	}
	return names
}
func notFound(result uintptr) bool {
	value := uint32(result)
	return value == 0x80320007 || value == 0x80320003 || value == 0x80320002 || value == 0x20000007 || value == 0x20000003 || value == 0x20000002
}
func isCode(err error, code uint32) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	value := uint32(errno)
	return value == code || value == 0x20000000|(code&0xff)
}
