//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
	"path/filepath"
	"testing"
)

// Orphan cleanup must not acquire authority from a registration or pin that
// an unprivileged principal can replace. This exercises real native security
// descriptors, independently of their filesystem/service publication producer.
func TestGenerationRemovalRejectsMutableOrForeignCustody(t *testing.T) {
	for _, test := range []struct {
		name, sddl     string
		native, refuse bool
	}{
		{"protected pin", "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)", false, false},
		{"mutable pin", "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FW;;;BU)", false, true},
		{"foreign owner", "O:BUD:P(A;;FA;;;SY)(A;;FA;;;BA)", false, true},
		{"native query", "O:SYD:(A;;GA;;;SY)(A;;GA;;;BA)(A;;CCLC;;;BU)", true, false},
		{"native reconfiguration", "O:SYD:(A;;GA;;;SY)(A;;GA;;;BA)(A;;DC;;;BU)", true, true},
		{"native security replacement", "O:SYD:(A;;GA;;;SY)(A;;GA;;;BA)(A;;WD;;;BU)", true, true},
		{"absent DACL", "O:SY", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = generationRemovalCustody(sd, test.native)
			if test.refuse {
				if !errors.Is(err, ErrServiceOwnership) {
					t.Fatalf("untrusted ownership accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Explicit opt-in read-only qualification of the surviving task-owned native
// registration. This does not stop/delete services or stand in for public
// uninstall; it finds custody failures before building the next CLI artifact.
func TestNativeGenerationUninstallPreflight(t *testing.T) {
	if *nativeSSHGeneration == "" || *nativeQueryOwner == "" {
		t.Skip("explicit native account generation required")
	}
	instance, err := service.WindowsUserInstance(*nativeQueryOwner)
	if err != nil {
		t.Fatal("invalid fixture account")
	}
	_, path, err := generationPaths(*nativeQueryOwner, *nativeSSHGeneration)
	if err != nil {
		t.Fatal("invalid fixture generation")
	}
	if err := generationStorage(path, *nativeQueryOwner, false); err != nil {
		t.Fatalf("protected storage preflight: %T", err)
	}
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatal("native manager unavailable")
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(ServiceName + "-" + instance + "-" + *nativeSSHGeneration)
	if err != nil {
		t.Fatal("fixture native registration unavailable")
	}
	defer item.Close()
	if err := generationRemovalRegistration(context.Background(), item, *nativeQueryOwner, instance, *nativeSSHGeneration, path); err != nil {
		t.Fatalf("registration/pin preflight: %T", err)
	}
}

func TestNativeSessionOwnerRemovalPreflight(t *testing.T) {
	if *nativeSSHGeneration == "" || *nativeQueryOwner == "" {
		t.Skip("explicit native account generation required")
	}
	instance, err := service.WindowsUserInstance(*nativeQueryOwner)
	if err != nil {
		t.Fatal("invalid fixture account")
	}
	name, err := service.WindowsSessionOwnerName(instance, *nativeSSHGeneration)
	if err != nil {
		t.Fatal("invalid fixture generation")
	}
	status, err := (service.WindowsController{}).Inspect(context.Background(), filepath.Join(`C:\ProgramData\Paperboat\services`, name+".json"))
	if err != nil {
		t.Fatalf("protected owner/native preflight: %T", err)
	}
	if !status.Registered {
		t.Fatal("fixture native registration absent")
	}
}
