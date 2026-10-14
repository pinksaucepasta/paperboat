//go:build windows

package wfp

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"testing"
	"unsafe"
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
