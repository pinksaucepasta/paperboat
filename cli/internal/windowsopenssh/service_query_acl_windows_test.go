//go:build windows

package windowsopenssh

import (
	"context"
	"flag"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

func TestServiceOwnerQueryACLPreservesExistingRights(t *testing.T) {
	owner, _ := windows.StringToSid("S-1-5-21-111-222-333-1001")
	original := "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;CCLCSWLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)"
	descriptor, err := windows.SecurityDescriptorFromString(original)
	if err != nil {
		t.Fatal(err)
	}
	existing, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	merged, err := serviceOwnerQueryACL(existing, owner)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := serviceDACLString(merged)
	if err != nil {
		t.Fatal(err)
	}
	expectedDescriptor, err := windows.SecurityDescriptorFromString(original + "(A;;CCLC;;;" + owner.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	entries := func(s string) []string { parts := strings.Split(s, "(")[1:]; sort.Strings(parts); return parts }
	if !reflect.DeepEqual(entries(actual), entries(expectedDescriptor.String())) {
		t.Fatalf("service query grant changed unrelated permissions: %s", actual)
	}
	repeated, err := serviceOwnerQueryACL(merged, owner)
	if err != nil {
		t.Fatal(err)
	}
	repeatedString, err := serviceDACLString(repeated)
	if err != nil || repeatedString != actual {
		t.Fatal("service query grant is not idempotent")
	}
	instance, err := hostservice.WindowsUserInstance(owner.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validatedServiceQueryOwner(ServiceName+"-"+instance, owner.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := validatedServiceQueryOwner(ServiceName+"-u000000000000000000000000", owner.String()); err == nil {
		t.Fatal("foreign service accepted owner permission grant")
	}
}

var nativeQueryOwner = flag.String("task39-ssh-owner", "", "task native SSH service owner SID")
var nativeQueryService = flag.String("task39-ssh-service", "", "task native SSH service name")
var nativeQueryWrapper = flag.String("task39-ssh-wrapper", "", "task native SSH wrapper path")
var nativeQueryBinary = flag.String("task39-ssh-binary", "", "task native sshd path")
var nativeQueryConfig = flag.String("task39-ssh-config", "", "task native sshd config path")

// Run elevated against only an explicit task-owned existing installation. This
// uses the production install/repair permission path; the caller then executes
// CheckLoopbackHealth as the actual unelevated owner in a separate process.
func TestNativeManagedSSHOwnerServiceQueryAccess(t *testing.T) {
	if *nativeQueryOwner == "" {
		t.Skip("explicit task-owned native service required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	if err := InstallService(ctx, *nativeQueryService, *nativeQueryWrapper, *nativeQueryBinary, *nativeQueryConfig, *nativeQueryOwner); err != nil {
		t.Fatal(err)
	}
}
