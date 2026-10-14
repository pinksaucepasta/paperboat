//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

func TestNativeWindowsSharedPurgeRetiresOnlyOwnedResidue(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated native filesystem custody")
	}
	const instance = "u111111111111111111111111"
	for _, scenario := range []string{"empty inherited scaffolding", "foreign and other account", "held diagnostic", "diagnostic hardlink", "mutable diagnostic", "reparse diagnostic", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			program := filepath.Join(root, "program")
			state := filepath.Join(root, "state")
			for _, p := range []string{program, state} {
				if err := secureSharedProgramRoot(p); err != nil {
					t.Fatal(err)
				}
			}
			shared := service.Layout{InstallRoot: program, UpdateStateRoot: filepath.Join(state, "updated")}
			for _, p := range []string{filepath.Join(program, "users"), filepath.Join(state, "users"), filepath.Join(state, "services"), filepath.Join(shared.UpdateStateRoot, "users")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(state, "PaperboatLocalDaemon-"+instance+"-startup-error.log")
			if err := os.WriteFile(log, []byte("owned diagnostic fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(state, "foreign.txt")
			if scenario == "foreign and other account" {
				if err := os.WriteFile(foreign, []byte("foreign fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, p := range []string{filepath.Join(program, "users", "u222222222222222222222222"), filepath.Join(state, "users", "u222222222222222222222222")} {
					if err := os.Mkdir(p, 0700); err != nil {
						t.Fatal(err)
					}
				}
				foreignLog := filepath.Join(state, "PaperboatLocalDaemon-u222222222222222222222222-startup-error.log")
				if err := os.WriteFile(foreignLog, []byte("other owner"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var held windows.Handle
			switch scenario {
			case "held diagnostic":
				var err error
				held, err = windows.CreateFile(windows.StringToUTF16Ptr(log), windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if held != 0 {
						windows.CloseHandle(held)
					}
				})
			case "diagnostic hardlink":
				if err := os.Link(log, foreign); err != nil {
					t.Fatal(err)
				}
			case "mutable diagnostic":
				descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;BU)")
				if err != nil {
					t.Fatal(err)
				}
				acl, _, err := descriptor.DACL()
				if err != nil {
					t.Fatal(err)
				}
				if err := windows.SetNamedSecurityInfo(log, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
					t.Fatal(err)
				}
			case "reparse diagnostic":
				if err := os.Rename(log, foreign); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, log); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			if scenario == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := purgeWindowsSharedResidue(ctx, shared, state, instance)
			switch scenario {
			case "empty inherited scaffolding":
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range []string{program, state} {
					if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("empty shared root retained: %v", err)
					}
				}
				if err := purgeWindowsSharedResidue(ctx, shared, state, instance); err != nil {
					t.Fatalf("idempotent purge: %v", err)
				}
			case "foreign and other account":
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned diagnostic retained: %v", err)
				}
				for _, p := range []string{foreign, filepath.Join(program, "users", "u222222222222222222222222"), filepath.Join(state, "users", "u222222222222222222222222"), filepath.Join(state, "PaperboatLocalDaemon-u222222222222222222222222-startup-error.log")} {
					if _, err := os.Stat(p); err != nil {
						t.Fatalf("foreign item changed: %v", err)
					}
				}
			default:
				if err == nil {
					t.Fatal("unsafe or unavailable custody was accepted")
				}
				if scenario == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if _, err := os.Lstat(log); err != nil {
					t.Fatalf("refused diagnostic changed: %v", err)
				}
				if scenario == "diagnostic hardlink" || scenario == "reparse diagnostic" {
					if body, err := os.ReadFile(foreign); err != nil || string(body) != "owned diagnostic fixture" {
						t.Fatalf("foreign target changed: %v", err)
					}
				}
				if held != 0 {
					if err := windows.CloseHandle(held); err != nil {
						t.Fatal(err)
					}
					held = 0
					if err := purgeWindowsSharedResidue(t.Context(), shared, state, instance); err != nil {
						t.Fatalf("held file recovery: %v", err)
					}
				}
			}
		})
	}
}
