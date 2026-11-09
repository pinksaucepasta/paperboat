//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

func TestNativeWindowsSharedPurgeRetiresOnlyOwnedResidue(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated native filesystem custody")
	}
	const instance = "u111111111111111111111111"
	// A different account than the one running the test, so its own SID earns the fixtures nothing.
	const ownerSID = "S-1-5-21-49157-49158-49159-1001"
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
			installLog := filepath.Join(state, "PaperboatInstall-"+instance+"-startup-error.log")
			if scenario == "foreign and other account" {
				if err := os.WriteFile(installLog, []byte("install failure cause"), 0600); err != nil {
					t.Fatal(err)
				}
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
			err := purgeWindowsSharedResidue(ctx, shared, state, instance, ownerSID)
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
				if err := purgeWindowsSharedResidue(ctx, shared, state, instance, ownerSID); err != nil {
					t.Fatalf("idempotent purge: %v", err)
				}
			case "foreign and other account":
				if err != nil {
					t.Fatal(err)
				}
				for _, owned := range []string{log, installLog} {
					if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("owned diagnostic retained: %s: %v", filepath.Base(owned), err)
					}
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
					if err := purgeWindowsSharedResidue(t.Context(), shared, state, instance, ownerSID); err != nil {
						t.Fatalf("held file recovery: %v", err)
					}
				}
			}
		})
	}
}

// victus, 2026-11-10: after a failed in-place update `pb uninstall` removed the services and Program Files but
// reported {"state":"failed","cause":"internal"}, kept the user's credentials and left one ProgramData log. The
// failed install had written PaperboatInstall-<instance>-startup-error.log with atomicfile's descriptor (full
// access for the installing account), which the shared cleanup refused as an untrusted writer.
func TestNativeWindowsSharedPurgeRemovesTheAccountsOwnInstallLog(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated native filesystem custody")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	const instance = "u111111111111111111111111"
	// The cleanup compares final paths, so the fixture must not sit behind a short (8.3) name.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	program, state := filepath.Join(root, "program"), filepath.Join(root, "state")
	for _, p := range []string{program, state} {
		if err := secureSharedProgramRoot(p); err != nil {
			t.Fatal(err)
		}
	}
	shared := service.Layout{InstallRoot: program, UpdateStateRoot: filepath.Join(state, "updated")}
	log := filepath.Join(state, "PaperboatInstall-"+instance+"-startup-error.log")
	// Written the way the elevated installer records its failure (causetext.Record).
	if err := atomicfile.Write(log, []byte("install failure cause"), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		t.Fatal(err)
	}
	err = purgeWindowsSharedResidue(t.Context(), shared, state, instance, "S-1-5-21-49157-49158-49159-1001")
	if err == nil {
		t.Fatal("another account's purge removed a log this account can write")
	}
	if detail := FailureDetail(err); !strings.Contains(detail, "remove the PaperboatInstall startup log") {
		t.Fatalf("the refusal does not name its log: %q", detail)
	}
	if _, err := os.Lstat(log); err != nil {
		t.Fatalf("refused log changed: %v", err)
	}
	if err := purgeWindowsSharedResidue(t.Context(), shared, state, instance, user.User.Sid.String()); err != nil {
		t.Fatalf("the account's own install log blocked its purge: %v (%s)", err, FailureDetail(err))
	}
	for _, p := range []string{log, state, program} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still there: %v", filepath.Base(p), err)
		}
	}
}

// The same rule on descriptors alone, so it needs neither elevation nor files.
func TestSharedProgramObjectDescriptorAcceptsOnlyTheAccountItself(t *testing.T) {
	const account, other = "S-1-5-21-49157-49158-49159-1001", "S-1-5-21-49157-49158-49159-1002"
	for _, row := range []struct {
		name, sddl, accountSID string
		accepted               bool
	}{
		{"service log", "O:SYD:(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", "", true},
		{"install log of this account", "O:" + account + "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + account + ")", account, true},
		{"install log written as Administrators", "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + account + ")", account, true},
		{"install log, no account named", "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + account + ")", "", false},
		{"install log of another account", "O:" + other + "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + other + ")", account, false},
		{"owned by another account", "O:" + other + "D:P(A;;FA;;;SY)(A;;FA;;;BA)", account, false},
		{"writable for all users", "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + account + ")(A;;FA;;;BU)", account, false},
	} {
		descriptor, err := windows.SecurityDescriptorFromString(row.sddl)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if err := checkSharedProgramObjectDescriptor(descriptor, false, row.accountSID); (err == nil) != row.accepted {
			t.Errorf("%s: accepted=%v, want %v (%v)", row.name, err == nil, row.accepted, err)
		}
	}
}
