//go:build windows

package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestSharedLockCreatesProtectedOwnerAndLockMetadata(t *testing.T) {
	// t.TempDir itself may be BA-owned when this native test is launched
	// through elevated SSH. Create the profile root with the same explicit
	// owner path as production before exercising missing lock parents.
	root := filepath.Join(t.TempDir(), "secure-root")
	if err := createSharedLockDirectory(root); err != nil {
		t.Fatal(err)
	}
	lock := newSharedLock(filepath.Join(root, "profiles", "profile.json.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			t.Error(err)
		}
	}()

	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl, err := currentUserCredentialSDDL()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(lock.path), lock.path} {
		handle, err := openSharedLockDirectory(path, false)
		if err != nil {
			t.Fatalf("open protected lock directory %s: %v", path, err)
		}
		if !windowssecurity.HandleOwnerMatchesSID(handle, owner) {
			t.Errorf("lock directory %s owner is not the stable user SID", path)
		}
		if !windowssecurity.ProtectedHandleDACLMatches(handle, sddl) {
			t.Errorf("lock directory %s DACL is not the exact protected credential DACL", path)
		}
		windows.CloseHandle(handle)
	}

	ownerPath := filepath.Join(lock.path, "owner.json")
	pathUTF16, err := windows.UTF16PtrFromString(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pathUTF16, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		t.Fatal(err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || information.NumberOfLinks != 1 {
		t.Fatalf("owner metadata has unsafe file attributes: %#x", information.FileAttributes)
	}
	if !windowssecurity.HandleOwnerMatchesSID(handle, owner) {
		t.Fatal("owner metadata owner is not the stable user SID")
	}
	if !windowssecurity.ProtectedHandleDACLMatches(handle, sddl) {
		t.Fatal("owner metadata DACL is not the exact protected credential DACL")
	}
	if _, err := os.Stat(ownerPath); err != nil {
		t.Fatal(err)
	}
}

func TestPendingRevocationMutationMigratesLegacyNetworkDirectoryBeforeLock(t *testing.T) {
	credentialDirectory, sddl := preparePrivateWindowsCredentialDirectory(t)
	store := ProfileStore{Path: credentialDirectory}
	record := PendingRevocation{
		Version:            ProfileVersion,
		Issuer:             "https://api.example.com",
		CLIClientSessionID: "cls_native_legacy_pending",
		RefreshSecretRef:   "revocation-native-legacy-pending-refresh",
		CreatedAt:          time.Now().UTC(),
	}
	pendingDirectory := filepath.Join(credentialDirectory, "pending-revocations")
	if err := ensureDPAPIDirectory(pendingDirectory, sddl); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := store.pendingRevocationPath(record.Issuer, record.CLIClientSessionID)
	if err := atomicWrite(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	makeWindowsNetworkTokenDirectoryFixture(t, pendingDirectory, false)
	updated, err := store.MarkRevocationSucceeded(record)
	if err != nil {
		t.Fatalf("mark revocation beneath legacy network-token directory: %v", err)
	}
	if !updated.ServerRevoked {
		t.Fatal("revocation was not marked as server-revoked")
	}
	if !credentialFilePrivate(pendingDirectory) {
		t.Fatal("legacy network-token pending directory was not migrated before locking")
	}
	if _, err := os.Lstat(newSharedLock(path + ".lock").path); !os.IsNotExist(err) {
		t.Fatalf("pending record lock remained after mutation: %v", err)
	}
}

// TEMP often holds the 8.3 spelling of a path; the lock parent check compares objects, not spellings.
func TestSharedLockAcceptsAnEightDotThreePathSpelling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a directory with a long name")
	if err := createSharedLockDirectory(root); err != nil {
		t.Fatal(err)
	}
	encoded, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(encoded, &buffer[0], uint32(len(buffer)))
	if err != nil || n == 0 {
		t.Skipf("short names are disabled on this volume: %v", err)
	}
	short := windows.UTF16ToString(buffer[:n])
	if short == root {
		t.Skip("the volume produced no distinct short name")
	}
	lock := newSharedLock(filepath.Join(short, "profile.json.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock under the short spelling %s: %v", short, err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

// setFixtureDefaultOwnerToAdministrators gives the process the default owner an elevated token has, so plain
// directory creation makes Administrators the owner, as it does in an Administrator PowerShell.
func setFixtureDefaultOwnerToAdministrators(t *testing.T) {
	t.Helper()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	owner := struct{ Owner *windows.SID }{Owner: administrators}
	if err := windows.SetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
		t.Skipf("Administrators as default owner needs an elevated token: %v", err)
	}
	t.Cleanup(func() {
		if err := setDefaultOwnerToCurrentUser(); err != nil {
			t.Error(err)
		}
	})
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(probe, administrators) {
		t.Fatal("fixture token did not make Administrators the default owner")
	}
}

func assertSharedLockParentPrivate(t *testing.T, path string) {
	t.Helper()
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(path, owner) || !credentialFilePrivate(path) {
		t.Fatalf("%s is not owned by the current user with the protected credential DACL", path)
	}
}

func TestConfigDirectoryCreatedFromElevatedTokenIsOwnedByTheUser(t *testing.T) {
	setFixtureDefaultOwnerToAdministrators(t)
	root := filepath.Join(t.TempDir(), "root")
	if err := createSharedLockDirectory(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(root, "paperboat", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	assertSharedLockParentPrivate(t, filepath.Dir(cfg.Path()))
	unlock, err := cfg.LockLocalAccess()
	if err != nil {
		t.Fatalf("lock beside a config saved from an elevated token: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedLockParentOwnedByAdministratorsIsRepairedToTheUser(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(map[bool]string{false: "protected", true: "inherited"}[inherited], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			if err := createSharedLockDirectory(root); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(root, "paperboat")
			if inherited {
				// What a plain MkdirAll under an elevated token leaves below a profile directory: SYSTEM,
				// Administrators and the user with full control, all inherited.
				owner, err := currentUserSID()
				if err != nil {
					t.Fatal(err)
				}
				setWindowsFixtureSecurity(t, root, owner, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;"+owner.String()+")", true)
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := createSharedLockDirectory(directory); err != nil {
				t.Fatal(err)
			}
			changeFixtureOwnerToAdministrators(t, directory)
			cfg, err := Load(filepath.Join(directory, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := cfg.LockLocalAccess()
			if err != nil {
				t.Fatalf("lock beside an Administrators-owned config directory: %v", err)
			}
			if err := unlock(); err != nil {
				t.Fatal(err)
			}
			assertSharedLockParentPrivate(t, directory)
		})
	}
}

func TestSharedLockParentWithAForeignOwnerIsRefusedUnchanged(t *testing.T) {
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	sddl, err := currentUserCredentialSDDL()
	if err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]func(t *testing.T, path string) *windows.SID{
		"another owner": func(t *testing.T, path string) *windows.SID {
			if err := windowssecurity.WithRestorePrivilege(func() error {
				return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, users, nil, nil, nil)
			}); err != nil {
				t.Skipf("assigning another owner needs an elevated token: %v", err)
			}
			return users
		},
		"Administrators with an extra ACE": func(t *testing.T, path string) *windows.SID {
			administrators := changeFixtureOwnerToAdministrators(t, path)
			setWindowsFixtureSecurity(t, path, administrators, sddl+"(A;;FR;;;WD)", true)
			return administrators
		},
	} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "paperboat")
			if err := createSharedLockDirectory(directory); err != nil {
				t.Fatal(err)
			}
			foreign := fixture(t, directory)
			cfg, err := Load(filepath.Join(directory, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cfg.LockLocalAccess(); !errors.Is(err, ErrCredentialStoreUnavailable) {
				t.Fatalf("lock beside a foreign-owned directory: %v", err)
			}
			if !windowssecurity.OwnerMatchesSID(directory, foreign) {
				t.Fatal("refused directory owner was changed")
			}
			if _, err := os.Lstat(filepath.Join(directory, "config.json.local-access.lock.d")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lock was created in a refused directory: %v", err)
			}
		})
	}
}
