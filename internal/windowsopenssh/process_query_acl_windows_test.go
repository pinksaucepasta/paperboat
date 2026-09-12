//go:build windows

package windowsopenssh

import (
	"context"
	"flag"
	"golang.org/x/sys/windows"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSSHDProcessOwnerQueryACLPreservesOtherRights(t *testing.T) {
	owner, _ := windows.StringToSid("S-1-5-21-111-222-333-1001")
	original := "D:(A;;GA;;;SY)(A;;GA;;;BA)"
	descriptor, err := windows.SecurityDescriptorFromString(original)
	if err != nil {
		t.Fatal(err)
	}
	existing, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := sshdProcessOwnerQueryACL(existing, owner)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := serviceDACLString(merged)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windows.SecurityDescriptorFromString(original + "(A;;0x1000;;;" + owner.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	entries := func(s string) []string { parts := strings.Split(s, "(")[1:]; sort.Strings(parts); return parts }
	if !reflect.DeepEqual(entries(actual), entries(expected.String())) {
		t.Fatalf("unexpected process privileges: %s", actual)
	}
	twice, err := sshdProcessOwnerQueryACL(merged, owner)
	if err != nil {
		t.Fatal(err)
	}
	twiceString, err := serviceDACLString(twice)
	if err != nil || twiceString != actual {
		t.Fatal("process query grant not idempotent")
	}
}

var nativeQueryPort = flag.Uint("task39-ssh-port", 0, "task native managed SSH listener port")

func TestNativeManagedSSHOwnerProcessQueryAccess(t *testing.T) {
	if *nativeQueryOwner == "" {
		t.Skip("explicit task-owned native service required")
	}
	if *nativeQueryPort == 0 || *nativeQueryPort > 65535 {
		t.Fatal("explicit valid task listener port required")
	}
	if _, err := validatedServiceQueryOwner(*nativeQueryService, *nativeQueryOwner); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig(nil)
	config.ServiceName = *nativeQueryService
	config.OwnerSID = *nativeQueryOwner
	config.ServiceExecutable = *nativeQueryWrapper
	config.InstallRoot = filepath.Dir(*nativeQueryBinary)
	config.StateRoot = filepath.Dir(*nativeQueryConfig)
	config.Port = uint16(*nativeQueryPort)
	result := Result{Port: config.Port, SSHDPath: *nativeQueryBinary, ConfigPath: *nativeQueryConfig}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	health, err := CheckLoopbackHealth(ctx, config, result)
	if err != nil {
		t.Fatal(err)
	}
	if !validLoopbackServiceCommand(config, result, health.Service.PathName) {
		t.Fatal(ErrServiceOwnership)
	}
	wrapper, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, health.Service.ProcessID)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(wrapper)
	image := func(handle windows.Handle) string {
		t.Helper()
		buffer := make([]uint16, 32768)
		size := uint32(len(buffer))
		if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil || size == 0 || size > uint32(len(buffer)) {
			t.Fatal("could not verify exact process image")
		}
		return windows.UTF16ToString(buffer[:size])
	}
	if !sameCleanPath(image(wrapper), config.ServiceExecutable) {
		t.Fatal(ErrServiceOwnership)
	}
	seen := map[uint32]bool{}
	for _, listener := range health.Listeners {
		if seen[listener.ProcessID] {
			continue
		}
		seen[listener.ProcessID] = true
		if listener.ParentProcessID != health.Service.ProcessID || listener.ProcessID == health.Service.ProcessID {
			t.Fatal(ErrServiceOwnership)
		}
		child, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.READ_CONTROL|windows.WRITE_DAC, false, listener.ProcessID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { windows.CloseHandle(child) })
		if !sameCleanPath(image(child), result.SSHDPath) {
			t.Fatal(ErrServiceOwnership)
		}
		parents, err := nativeListenerParents(map[uint32]windows.Handle{listener.ProcessID: child})
		if err != nil || parents[listener.ProcessID] != health.Service.ProcessID {
			t.Fatal("listener parent changed before permission grant")
		}
		current, err := queryLoopbackService(config.ServiceName)
		if err != nil || current.ProcessID != health.Service.ProcessID || !validLoopbackServiceCommand(config, result, current.PathName) {
			t.Fatal("service ownership changed before permission grant")
		}
		if err := grantSSHDProcessOwnerQuery(child, config.ServiceName, config.OwnerSID); err != nil {
			t.Fatal(err)
		}
	}
}
