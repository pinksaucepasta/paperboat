//go:build windows

package wfp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Real WFP objects with unique sublayers and a single unused loopback address.
// No installed guard or foreign system filter is changed by this fixture.
func TestScopedRemovalHandlesLostJournalAndPreservesForeignFilters(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated native WFP access")
	}
	var engine uintptr
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, nil, unsafe.Pointer(&engine)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fwpmEngineClose0(engine); err != nil {
			t.Error(err)
		}
	})
	keys := make([]windows.GUID, 2)
	filters := make([]windows.GUID, 2)
	for i := range keys {
		var err error
		keys[i], err = windows.GenerateGUID()
		if err != nil {
			t.Fatal(err)
		}
		filters[i], err = windows.GenerateGUID()
		if err != nil {
			t.Fatal(err)
		}
		display, err := createWtFwpmDisplayData0("Paperboat scoped cleanup fixture", "")
		if err != nil {
			t.Fatal(err)
		}
		sub := wtFwpmSublayer0{subLayerKey: keys[i], displayData: *display, weight: 1}
		if err := fwpmSubLayerAdd0(engine, &sub, 0); err != nil {
			t.Fatal(err)
		}
		key, filterKey := keys[i], filters[i]
		t.Cleanup(func() {
			r, _, _ := filterDeleteProc.Call(engine, uintptr(unsafe.Pointer(&filterKey)))
			if r != 0 && !notFound(r) {
				t.Error(windows.Errno(r))
			}
			r, _, _ = sublayerDeleteProc.Call(engine, uintptr(unsafe.Pointer(&key)))
			if r != 0 && !notFound(r) {
				t.Error(windows.Errno(r))
			}
		})
		conditions := []wtFwpmFilterCondition0{uint32Condition(cFWPM_CONDITION_IP_REMOTE_ADDRESS, 0x7ffefffe), uint16Condition(cFWPM_CONDITION_IP_REMOTE_PORT, 65534)}
		filter := wtFwpmFilter0{filterKey: filters[i], displayData: *display, layerKey: cFWPM_LAYER_ALE_AUTH_CONNECT_V4, subLayerKey: keys[i], weight: filterWeight(1), numFilterConditions: uint32(len(conditions)), filterCondition: &conditions[0], action: wtFwpmAction0{_type: cFWP_ACTION_PERMIT}}
		var id uint64
		if err := fwpmFilterAdd0(engine, &filter, 0, &id); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(key windows.GUID) bool {
		var filter *wtFwpmFilter0
		proc := windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmFilterGetByKey0")
		r, _, _ := proc.Call(engine, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&filter)))
		if filter != nil {
			fwpmFreeMemory0(unsafe.Pointer(&filter))
		}
		if r == 0 {
			return true
		}
		if !notFound(r) {
			t.Fatal(windows.Errno(r))
		}
		return false
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runTransaction(engine, func(e uintptr) error { return removeSublayerFilters(canceled, e, keys[:1]) }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if !exists(filters[0]) || !exists(filters[1]) {
		t.Fatal("canceled cleanup changed filters")
	}
	// No lease names or journal are supplied: sublayer ownership is authoritative.
	if err := runTransaction(engine, func(e uintptr) error { return removeSublayerFilters(t.Context(), e, keys[:1]) }); err != nil {
		t.Fatal(err)
	}
	if exists(filters[0]) || !exists(filters[1]) {
		t.Fatal("owned filter retained or foreign filter removed")
	}
}

// Apply replaces the whole policy of the sublayer: a lease that is no longer in the desired set leaves no filter
// behind, so access ends with the lease. Uses its own sublayer, so an installed guard is not touched.
func TestApplyRetiresTheFiltersOfAWithdrawnLease(t *testing.T) {
	token := windows.GetCurrentProcessToken()
	if !token.IsElevated() {
		t.Skip("requires elevated native WFP access")
	}
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	prior := sublayerKey
	sublayerKey = guidFor(fmt.Sprintf("paperboat-apply-test-%d", os.Getpid()))
	t.Cleanup(func() {
		if err := Remove(context.Background()); err != nil {
			t.Error(err)
		}
		sublayerKey = prior
	})
	sid := user.User.Sid.String()
	first, second := Lease{IP: "127.100.254.71", SID: sid}, Lease{IP: "127.100.254.72", SID: sid}
	keyOf := func(l Lease) windows.GUID { return guidFor(l.IP + "\x00" + l.SID) }
	installed := func() []windows.GUID { return installedIn(t, sublayerKey) }
	// With no guard installed, a lease's permit is also visible as traffic: a connect to its address is let
	// through to the (empty) address and refused there, while a connect to an address without a permit is
	// blocked by the range's outbound block before it gets that far.
	probe := func(l Lease) bool {
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort(l.IP, "46399"), 2*time.Second)
		if err == nil {
			conn.Close()
			return false
		}
		return errors.Is(err, windows.WSAECONNREFUSED)
	}
	probing := len(installedIn(t, prior)) == 0
	apply := func(leases ...Lease) {
		t.Helper()
		if err := Apply(leases, "127.100.0.0/16"); err != nil {
			t.Fatal(err)
		}
	}

	apply(first, second)
	if keys := installed(); !slices.Contains(keys, keyOf(first)) || !slices.Contains(keys, keyOf(second)) {
		t.Fatalf("granted leases have no filter: %d filters installed", len(keys))
	}
	if probing && (!probe(first) || !probe(second)) {
		t.Fatal("a granted lease's address is not reachable for its user")
	}
	apply(second)
	if keys := installed(); slices.Contains(keys, keyOf(first)) || !slices.Contains(keys, keyOf(second)) {
		t.Fatalf("after withdrawing the first lease: first present=%v second present=%v", slices.Contains(keys, keyOf(first)), slices.Contains(keys, keyOf(second)))
	}
	if probing && (probe(first) || !probe(second)) {
		t.Fatalf("after withdrawing the first lease: first reachable=%v second reachable=%v", probe(first), probe(second))
	}
	apply()
	if keys := installed(); slices.Contains(keys, keyOf(first)) || slices.Contains(keys, keyOf(second)) {
		t.Fatal("a withdrawn lease left its filter installed")
	}
	if probing && (probe(first) || probe(second)) {
		t.Fatal("a withdrawn lease's address is still reachable for its user")
	}
}

// A user without a lease reaches the browser gateway's ports, so the gateway can answer it with the refusal page
// (X-44); every other address of the range stays blocked for it.
func TestApplyLetsAnyUserConnectToTheGateway(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated native WFP access")
	}
	assertGatewayConnectPolicy(t)
	prior := sublayerKey
	if len(installedIn(t, prior)) != 0 {
		// Real traffic is decided by every sublayer, so beside an installed guard it says nothing about this policy.
		t.Log("a guard is installed: checked the policy only")
		return
	}
	sublayerKey = guidFor(fmt.Sprintf("paperboat-gateway-test-%d", os.Getpid()))
	t.Cleanup(func() {
		if err := Remove(context.Background()); err != nil {
			t.Error(err)
		}
		sublayerKey = prior
	})
	if err := Apply(nil, "127.100.0.0/16"); err != nil {
		t.Fatal(err)
	}
	denied := func(address string) bool {
		conn, err := net.DialTimeout("tcp4", address, 2*time.Second)
		if err == nil {
			conn.Close()
			return false
		}
		return errors.Is(err, windows.WSAEACCES)
	}
	if denied("127.100.0.1:80") || denied("127.100.0.1:443") {
		t.Fatal("a user without a lease is blocked from the gateway: no refusal page can reach it")
	}
	if !denied("127.100.0.9:80") {
		t.Fatal("the gateway permit opened another address of the range")
	}
}

// assertGatewayConnectPolicy reads back, inside a transaction it aborts, the outbound-connect policy Apply installs
// with no lease: permits for the gateway's two ports with no user condition, above a block of the whole range, and no
// other permit. Nothing is ever enforced and an installed guard stays as it was. It uses the product sublayer because
// filter keys are unique across the filter engine: beside an installed guard, a copy in another sublayer is refused as
// already existing.
func assertGatewayConnectPolicy(t *testing.T) {
	t.Helper()
	policy, err := desiredPolicy(nil, "127.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	var engine uintptr
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, nil, unsafe.Pointer(&engine)); err != nil {
		t.Fatal(err)
	}
	defer fwpmEngineClose0(engine)
	before := installedIn(t, sublayerKey)
	getByKey := windows.NewLazySystemDLL("fwpuclnt.dll").NewProc("FwpmFilterGetByKey0")
	var permits, blocks []string
	var lowestPermit, highestBlock uintptr = 255, 0
	inspected := errors.New("inspected")
	err = runTransaction(engine, func(engine uintptr) error {
		if err := policy(engine); err != nil {
			return err
		}
		keys, err := sublayerFilterKeys(t.Context(), engine, []windows.GUID{sublayerKey})
		if err != nil {
			return err
		}
		for i := range keys {
			var filter *wtFwpmFilter0
			if result, _, _ := getByKey.Call(engine, uintptr(unsafe.Pointer(&keys[i])), uintptr(unsafe.Pointer(&filter))); result != 0 {
				return windows.Errno(result)
			}
			if filter.layerKey == cFWPM_LAYER_ALE_AUTH_CONNECT_V4 {
				var described []string
				for _, condition := range unsafe.Slice(filter.filterCondition, int(filter.numFilterConditions)) {
					value := condition.conditionValue.value
					switch condition.fieldKey {
					case cFWPM_CONDITION_IP_PROTOCOL:
						described = append(described, fmt.Sprintf("protocol=%d", uint8(value)))
					case cFWPM_CONDITION_IP_REMOTE_PORT:
						described = append(described, fmt.Sprintf("port=%d", uint16(value)))
					case cFWPM_CONDITION_IP_REMOTE_ADDRESS:
						if condition.conditionValue._type == cFWP_V4_ADDR_MASK {
							masked := *(**wtFwpV4AddrAndMask)(unsafe.Pointer(&value))
							described = append(described, fmt.Sprintf("range=%08x/%08x", masked.addr, masked.mask))
						} else {
							described = append(described, fmt.Sprintf("address=%08x", uint32(value)))
						}
					default:
						described = append(described, "other="+condition.fieldKey.String())
					}
				}
				slices.Sort(described)
				rule := strings.Join(described, " ")
				if filter.action._type == cFWP_ACTION_PERMIT {
					permits, lowestPermit = append(permits, rule), min(lowestPermit, filter.weight.value)
				} else {
					blocks, highestBlock = append(blocks, rule), max(highestBlock, filter.weight.value)
				}
			}
			fwpmFreeMemory0(unsafe.Pointer(&filter))
		}
		return inspected
	})
	if !errors.Is(err, inspected) {
		t.Fatal(err)
	}
	slices.Sort(permits)
	wantPermits := []string{"address=7f640001 port=443 protocol=6", "address=7f640001 port=80 protocol=6"}
	if !slices.Equal(permits, wantPermits) {
		t.Fatalf("connect permits = %q, want only the gateway's ports for any user: %q", permits, wantPermits)
	}
	if want := []string{"protocol=6 range=7f640000/ffff0000"}; !slices.Equal(blocks, want) {
		t.Fatalf("connect blocks = %q, want %q", blocks, want)
	}
	if lowestPermit <= highestBlock {
		t.Fatalf("gateway permit weight %d does not outrank the range block %d", lowestPermit, highestBlock)
	}
	if after := installedIn(t, sublayerKey); !slices.Equal(after, before) {
		t.Fatalf("reading the policy changed the installed filters: %d before, %d after", len(before), len(after))
	}
}

// installedIn lists the filter keys installed in one sublayer.
func installedIn(t *testing.T, sublayer windows.GUID) []windows.GUID {
	t.Helper()
	var engine uintptr
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, nil, unsafe.Pointer(&engine)); err != nil {
		t.Fatal(err)
	}
	defer fwpmEngineClose0(engine)
	keys, err := sublayerFilterKeys(context.Background(), engine, []windows.GUID{sublayer})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// One invalid lease must not fail the transaction for the valid ones: the valid lease keeps its filter, the invalid
// ones (bad address, bad SID) get none, and Apply succeeds.
func TestApplySkipsAnInvalidLeaseAndInstallsTheRest(t *testing.T) {
	token := windows.GetCurrentProcessToken()
	if !token.IsElevated() {
		t.Skip("requires elevated native WFP access")
	}
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	prior := sublayerKey
	sublayerKey = guidFor(fmt.Sprintf("paperboat-skip-test-%d", os.Getpid()))
	t.Cleanup(func() {
		if err := Remove(context.Background()); err != nil {
			t.Error(err)
		}
		sublayerKey = prior
	})
	sid := user.User.Sid.String()
	good := Lease{IP: "127.100.254.81", SID: sid}
	badIP, badSID := Lease{IP: "not-an-address", SID: sid}, Lease{IP: "127.100.254.82", SID: "not-a-sid"}
	if err := Apply([]Lease{badIP, good, badSID}, "127.100.0.0/16"); err != nil {
		t.Fatalf("one invalid lease failed the whole policy: %v", err)
	}
	keys := installedIn(t, sublayerKey)
	key := func(l Lease) windows.GUID { return guidFor(l.IP + "\x00" + l.SID) }
	if !slices.Contains(keys, key(good)) {
		t.Fatal("the valid lease has no filter")
	}
	if slices.Contains(keys, key(badIP)) || slices.Contains(keys, key(badSID)) {
		t.Fatal("an invalid lease got a filter")
	}
}
