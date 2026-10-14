//go:build windows

package hostinstall

import (
	"context"
	"crypto/rand"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

var pinCustodySource = flag.String("task23-pin-custody-source", "", "verified protected pb fixture for privileged publication custody regression")
var pinCustodyOwner = flag.String("task23-pin-custody-owner", "", "enrolled SID for isolated privileged publication custody regression")

func TestNativeWindowsPinPublisherProtectsVerifiedBytes(t *testing.T) {
	if *pinCustodySource == "" || *pinCustodyOwner == "" {
		t.Skip("requires a verified protected pb and enrolled SID fixture")
	}
	allowed, err := checkAdministrator()
	if err != nil || !allowed {
		t.Fatal("requires an elevated administrator or SYSTEM")
	}
	body, err := readInstallFile(*pinCustodySource, installsource.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	owner, _ := windowsRuntimeTrustedOwner()
	ba, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	for _, scenario := range []string{"verified", "wrong-digest", "mutable", "foreign-owner", "linked", "changed-bytes"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "pb.exe")
			if err := applyWindowsOwnedDACL(root, owner, windowsRuntimeCurrentRootDACL(*pinCustodyOwner)); err != nil {
				t.Fatal(err)
			}
			data := body
			if scenario == "changed-bytes" {
				image, err := pe.Open(*pinCustodySource)
				if err != nil {
					t.Fatal(err)
				}
				text := image.Section(".text")
				if text == nil {
					t.Fatal("fixture has no text section")
				}
				offset := int(text.Offset) + 16
				image.Close()
				if offset >= len(body) {
					t.Fatal("invalid fixture section")
				}
				data = append([]byte(nil), body...)
				data[offset] ^= 1
			}
			if err := os.WriteFile(path, data, 0755); err != nil {
				t.Fatal(err)
			}
			legacy := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"
			if err := applyWindowsOwnedDACL(path, owner, legacy); err != nil {
				t.Fatal(err)
			}
			source, err := installsource.Inspect(*pinCustodySource, "2026.10.12.0", installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "wrong-digest":
				source.SHA256 = strings.Repeat("0", 64)
			case "mutable":
				if err := applyWindowsOwnedDACL(path, owner, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;BU)"); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				if err := applyWindowsOwnedDACL(path, ba, legacy); err != nil {
					t.Fatal(err)
				}
			case "linked":
				if err := os.Link(path, filepath.Join(root, "other.exe")); err != nil {
					t.Fatal(err)
				}
			}
			err = protectVerifiedWindowsReleasePin(context.Background(), path, source, *pinCustodyOwner)
			current := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + *pinCustodyOwner + ")"
			if scenario == "verified" {
				if err != nil {
					t.Fatal(err)
				}
				if err := verifyWindowsSource(context.Background(), path, source, *pinCustodyOwner); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe pin adopted")
				}
				if windowssecurity.ProtectedDACLMatches(path, current) {
					t.Fatal("rejected pin custody changed")
				}
			}
		})
	}
}

func TestNativeWindowsPinPublisherProtectsBoundAccountNamespace(t *testing.T) {
	if *pinCustodySource == "" {
		t.Skip("requires a verified protected pb fixture")
	}
	allowed, err := checkAdministrator()
	if err != nil || !allowed {
		t.Fatal("requires an elevated administrator or SYSTEM")
	}
	body, err := readInstallFile(*pinCustodySource, installsource.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"verified", "mutable", "foreign-owner", "unbound", "changed-bytes", "extra-reader"} {
		t.Run(scenario, func(t *testing.T) {
			var nonce [12]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			sid := fmt.Sprintf("S-1-5-21-%d-%d-%d-1001", binary.LittleEndian.Uint32(nonce[0:4]), binary.LittleEndian.Uint32(nonce[4:8]), binary.LittleEndian.Uint32(nonce[8:12]))
			layout, err := service.WindowsUserLayout(sid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(layout.InstallRoot); !installNotExistOnly(err) {
				t.Fatal("fixture account namespace already exists")
			}
			t.Cleanup(func() {
				cleanupOwner, _ := windowsRuntimeTrustedOwner()
				if err := applyWindowsOwnedDACL(layout.InstallRoot, cleanupOwner, windowsRuntimeCurrentRootDACL(sid)); err != nil {
					t.Error(err)
				}
				if err := os.RemoveAll(layout.InstallRoot); err != nil {
					t.Error(err)
				}
			})
			previous := windowsProgramDataRoot
			windowsProgramDataRoot = filepath.Join(t.TempDir(), "Paperboat")
			defer func() { windowsProgramDataRoot = previous }()
			owner, _ := windowsRuntimeTrustedOwner()
			ba, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
			if err := os.MkdirAll(filepath.Dir(layout.Binary), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(layout.Binary, body, 0755); err != nil {
				t.Fatal(err)
			}
			if err := applyWindowsOwnedDACL(layout.Binary, owner, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;"+sid+")"); err != nil {
				t.Fatal(err)
			}
			source, err := installsource.Inspect(layout.Binary, "2026.10.12.0", installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := WindowsInstanceRoot(layout.Instance)
			token, _ := WindowsInstanceTokenPath(layout.Instance)
			if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
				t.Fatal(err)
			}
			if scenario != "unbound" {
				config := WindowsRuntimeConfig{Schema: windowsConfigSchema, Instance: layout.Instance, OwnerSID: sid, User: "custody-fixture", StateRoot: root, Workspace: root, ListenAddress: "127.0.0.1:8080", MachineID: "custody-fixture", TokenFile: token, Source: source, Artifact: bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1, Kind: bootstrap.ArtifactKindPB, Version: source.Version, Platform: "windows", Architecture: runtime.GOARCH, RepositoryURL: "https://fixture.invalid", TargetPath: "pb-windows-amd64.exe"}}
				if err := writeWindowsConfigAt(config, root); err != nil {
					t.Fatal(err)
				}
				if err := ensureWindowsTokenAt(token, sid); err != nil {
					t.Fatal(err)
				}
			}
			// Reproduce a BA-owned, unprotected inherited read-only account root.
			if err := windowssecurity.WithRestorePrivilege(func() error {
				return windows.SetNamedSecurityInfo(layout.InstallRoot, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, ba, nil, nil, nil)
			}); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "mutable":
				if err := applyWindowsOwnedDACL(layout.InstallRoot, ba, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;BU)"); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				foreign, _ := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
				if err := applyWindowsOwnedDACL(layout.InstallRoot, foreign, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"); err != nil {
					t.Fatal(err)
				}
			case "changed-bytes":
				data := append([]byte(nil), body...)
				data[len(data)-1] ^= 1
				if err := os.WriteFile(layout.Binary, data, 0755); err != nil {
					t.Fatal(err)
				}
			case "extra-reader":
				if err := applyWindowsOwnedDACL(layout.InstallRoot, ba, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;WD)"); err != nil {
					t.Fatal(err)
				}
			}
			err = prepareWindowsAccountPinRoot(context.Background(), layout, sid)
			if scenario == "verified" {
				if err != nil {
					t.Fatal(err)
				}
				if !windowssecurity.OwnerMatchesSID(layout.InstallRoot, owner) || !windowssecurity.ProtectedDACLMatches(layout.InstallRoot, windowsRuntimeCurrentRootDACL(sid)) {
					t.Fatal("bound namespace not protected")
				}
			} else {
				if err == nil {
					t.Fatal("untrusted namespace adopted")
				}
				if windowssecurity.OwnerMatchesSID(layout.InstallRoot, owner) && windowssecurity.ProtectedDACLMatches(layout.InstallRoot, windowsRuntimeCurrentRootDACL(sid)) {
					t.Fatal("rejected namespace custody changed")
				}
				if _, statErr := os.Stat(layout.Binary); errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("rejected namespace lost installed bytes")
				}
			}
		})
	}
}

// A native lifecycle transaction holds ancestors against rename while its
// installation callback ensures the already-protected machine root.
func TestNativeWindowsMachineDirectoryHeldCustody(t *testing.T) {
	if *pinCustodyOwner == "" {
		t.Skip("native owner fixture required")
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	expected := windowsRuntimeCurrentRootDACL(*pinCustodyOwner)
	if err := applyWindowsOwnedDACL(root, owner, expected); err != nil {
		t.Fatal(err)
	}
	ptr, _ := windows.UTF16PtrFromString(root)
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := ensureWindowsMachineDirectory(root, *pinCustodyOwner); err != nil {
		t.Fatal(err)
	}
	if err := checkWindowsRuntimeHandleSecurity(handle, owner, expected); err != nil {
		t.Fatal("held custody changed", err)
	}
}

func TestNativeWindowsSnapshotRestoresPromotedBytesBeforeMetadata(t *testing.T) {
	if *pinCustodySource == "" {
		t.Skip("verified pb fixture required")
	}
	allowed, err := checkAdministrator()
	if err != nil || !allowed {
		t.Fatal("elevated native fixture required")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	sid := fmt.Sprintf("S-1-5-21-%d-%d-%d-1001", binary.LittleEndian.Uint32(nonce[0:4]), binary.LittleEndian.Uint32(nonce[4:8]), binary.LittleEndian.Uint32(nonce[8:12]))
	layout, err := service.WindowsUserLayout(sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.InstallRoot); !installNotExistOnly(err) {
		t.Fatal("fixture namespace already exists")
	}
	owner, _ := windowsRuntimeTrustedOwner()
	t.Cleanup(func() {
		if err := os.RemoveAll(layout.InstallRoot); err != nil {
			t.Error(err)
		}
	})
	previous := windowsProgramDataRoot
	windowsProgramDataRoot = filepath.Join(t.TempDir(), "Paperboat")
	defer func() { windowsProgramDataRoot = previous }()
	body, err := readInstallFile(*pinCustodySource, installsource.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(*pinCustodySource, "2026.10.12.0", installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := WindowsInstanceRoot(layout.Instance)
	token, _ := WindowsInstanceTokenPath(layout.Instance)
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	config := WindowsRuntimeConfig{Schema: windowsConfigSchema, Instance: layout.Instance, OwnerSID: sid, User: "snapshot-fixture", StateRoot: root, Workspace: root, ListenAddress: "127.0.0.1:8080", MachineID: "snapshot-fixture", TokenFile: token, Source: source, Artifact: bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1, Kind: bootstrap.ArtifactKindPB, Version: source.Version, Platform: "windows", Architecture: runtime.GOARCH, RepositoryURL: "https://fixture.invalid", TargetPath: "pb-windows-amd64.exe"}}
	if err := writeWindowsConfigAt(config, root); err != nil {
		t.Fatal(err)
	}
	if err := ensureWindowsTokenAt(token, sid); err != nil {
		t.Fatal(err)
	}
	pin, err := WindowsPinnedSourcePath(layout, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{pin, layout.Binary} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := applyWindowsOwnedDACL(filepath.Dir(path), owner, windowsRuntimeCurrentRootDACL(sid)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0755); err != nil {
			t.Fatal(err)
		}
		if err := applyWindowsOwnedDACL(path, owner, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;"+sid+")"); err != nil {
			t.Fatal(err)
		}
	}
	state := windowsInstallationState{layout: layout}
	snapshot, err := state.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), body...)
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(layout.Binary, changed, 0755); err != nil {
		t.Fatal(err)
	}
	if source.Verify(layout.Binary) == nil {
		t.Fatal("promoted bytes match baseline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var decoded windowsInstallationSnapshot
	if err := json.Unmarshal(snapshot, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.InstallationAbsent || decoded.Source == nil || decoded.Config == nil {
		t.Fatal("baseline installation not captured")
	}
	loaded, loadErr := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	have, _ := json.Marshal(loaded)
	want, _ := json.Marshal(decoded.Config)
	t.Logf("configuration_equal=%t source_equal=%t", string(have) == string(want), loaded.Source == *decoded.Source)
	if err := state.Restore(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := verifyWindowsSource(ctx, layout.Binary, source, sid); err != nil {
		t.Fatal("baseline bytes not restored", err)
	}
	restored, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil || restored.Source != source {
		t.Fatal("baseline metadata changed", err)
	}
}

// Ordinary native recovery must refuse the real signed snapshot schema before
// touching installation bytes, even when a completion journal needs retiring.
func TestWindowsPreparedSnapshotRecoveryOwner(t *testing.T) {
	id := strings.Repeat("a", 64)
	body, err := json.Marshal(windowsInstallationSnapshot{PreparedCandidateID: id})
	if err != nil {
		t.Fatal(err)
	}
	generic := windowsInstallationState{}
	if err := generic.AuthorizeRecovery(context.Background(), body); !errors.Is(err, ErrSignedUpdatePending) {
		t.Fatalf("ordinary recovery admitted signed owner: %v", err)
	}
	if err := generic.Restore(context.Background(), body); !errors.Is(err, ErrSignedUpdatePending) {
		t.Fatalf("ordinary restore admitted signed owner: %v", err)
	}
	other := windowsInstallationState{preparedCandidateID: strings.Repeat("b", 64)}
	if err := other.AuthorizeRecovery(context.Background(), body); !errors.Is(err, ErrSignedUpdatePending) {
		t.Fatalf("different approval admitted signed owner: %v", err)
	}
	owner := windowsInstallationState{preparedCandidateID: id}
	if err := owner.AuthorizeRecovery(context.Background(), body); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("matching marker authorized malformed native snapshot: %v", err)
	}
	// Matching the marker alone does not authorize malformed native restoration.
	if err := owner.Restore(context.Background(), body); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed native snapshot restored: %v", err)
	}
	malformed, _ := json.Marshal(windowsInstallationSnapshot{PreparedCandidateID: "not-an-approval"})
	if err := generic.AuthorizeRecovery(context.Background(), malformed); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed approval accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := owner.AuthorizeRecovery(ctx, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled recovery accepted: %v", err)
	}
}
