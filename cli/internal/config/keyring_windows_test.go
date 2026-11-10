//go:build windows

package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func changeFixtureOwnerToAdministrators(t *testing.T, path string) *windows.SID {
	t.Helper()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("resolve current owner: %v", err)
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	if administrators.Equals(user.User.Sid) {
		t.Skip("current SID already equals the hostile owner fixture SID")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, administrators, nil, nil, nil); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skipf("changing the fixture owner requires an elevated native runner: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil)
	})
	return administrators
}

func preparePrivateWindowsCredentialDirectory(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("LOCALAPPDATA", t.TempDir())
	directory, err := dpapiCredentialDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	sddl, err := currentUserCredentialSDDL()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setWindowsFixtureSecurity(t, directory, owner, sddl, true)
	if !credentialFilePrivate(directory) {
		t.Fatal("credential parent fixture is not current-user owned and protected")
	}
	return directory, sddl
}

func setWindowsFixtureSecurity(t *testing.T, path string, owner *windows.SID, sddl string, protected bool) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		t.Fatal(err)
	}
	securityInformation := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protected {
		securityInformation = windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, securityInformation, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(absolute)
}

func makeWindowsNetworkTokenDirectoryFixture(t *testing.T, path string, extraACE bool) *windows.SID {
	t.Helper()
	administrators := changeFixtureOwnerToAdministrators(t, path)
	sddl := "D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;S-1-5-5-0-424242)"
	if extraACE {
		sddl += "(A;;FR;;;WD)"
	}
	setWindowsFixtureSecurity(t, path, administrators, sddl, false)
	return administrators
}

func TestWindowsFileFallbackIsDPAPIProtected(t *testing.T) {
	directory := t.TempDir()
	store := FileSecretStore{Dir: directory}
	ref, secret := "fallback-native", "must-not-appear-in-plaintext-नौका"
	if err := store.Set(ref, secret); err != nil {
		t.Fatalf("set protected fallback: %v", err)
	}
	body, err := os.ReadFile(store.path(ref))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Fatal("protected credential contains plaintext")
	}
	actual, err := store.Get(ref)
	if err != nil || actual != secret {
		t.Fatalf("get protected fallback = %q, %v", actual, err)
	}
}

func TestWindowsDPAPIAuthorityRoundTrip(t *testing.T) {
	ref := fmt.Sprintf("native-test-%d", time.Now().UnixNano())
	store := KeyringStore{}
	t.Cleanup(func() { _ = store.Delete(ref) })
	for _, value := range []string{"paperboat windows credential", "Unicode: नौका 🚤"} {
		if err := store.Set(ref, value); err != nil {
			t.Fatalf("set credential: %v", err)
		}
		actual, err := store.Get(ref)
		if err != nil || actual != value {
			t.Fatalf("get credential = %q, %v; want %q", actual, err, value)
		}
		path, _, err := dpapiSecretPath(ref)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		header := keyringDPAPIV2Header(ref, false)
		if err != nil || !bytes.HasPrefix(body, header) || bytes.Contains(body, []byte(value)) {
			t.Fatalf("credential is not a sealed machine-scope v2 envelope: header=%x err=%v", body[:min(len(body), len(header))], err)
		}
	}
	if err := store.Delete(ref); err != nil {
		t.Fatalf("delete credential: %v", err)
	}
	if _, err := store.Get(ref); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("read deleted credential: %v", err)
	}
	if err := store.Delete(ref); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func cleanupWindowsKeyringFixture(ref string) {
	_ = deleteDPAPISecret(ref)
}

func TestWindowsDPAPIV2RejectsTamperWrongScopeAndWrongRef(t *testing.T) {
	store := KeyringStore{}
	ref := fmt.Sprintf("native-v2-tamper-%d", time.Now().UnixNano())
	otherRef := ref + "-other"
	t.Cleanup(func() {
		_ = store.Delete(ref)
		_ = store.Delete(otherRef)
	})
	if err := store.Set(ref, "machine-secret"); err != nil {
		t.Fatal(err)
	}
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	wrongScope := append([]byte(nil), original...)
	wrongScope[5] = 2
	if err := os.WriteFile(path, wrongScope, 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get(ref); value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("wrong-scope envelope returned value=%q error=%v", value, err)
	}

	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get(ref); value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("tampered v2 envelope returned value=%q error=%v", value, err)
	}

	if err := store.Set(otherRef, "placeholder"); err != nil {
		t.Fatal(err)
	}
	otherPath, _, err := dpapiSecretPath(otherRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get(otherRef); value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("wrong-ref envelope returned value=%q error=%v", value, err)
	}
}

func TestWindowsMachineScopeCredentialMigratesTrustedAdministratorsOwnedDirectory(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	store := KeyringStore{}
	ref := fmt.Sprintf("native-trusted-legacy-owner-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = store.Delete(ref) })
	if err := store.Set(ref, "machine-secret"); err != nil {
		t.Fatal(err)
	}
	_, directory, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	changeFixtureOwnerToAdministrators(t, directory)
	if value, err := store.Get(ref); value != "machine-secret" || err != nil {
		t.Fatalf("trusted legacy credential directory returned value=%q error=%v", value, err)
	}
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(directory, owner) || !credentialFilePrivate(directory) {
		t.Fatal("trusted legacy credential directory was not migrated to the current owner and protected ACL")
	}
}

func TestWindowsMachineScopeCredentialMigratesTrustedAdministratorsOwnedFile(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	store := KeyringStore{}
	ref := fmt.Sprintf("native-trusted-legacy-file-owner-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = store.Delete(ref) })
	if err := store.Set(ref, "machine-secret"); err != nil {
		t.Fatal(err)
	}
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	changeFixtureOwnerToAdministrators(t, path)
	if value, err := store.Get(ref); value != "machine-secret" || err != nil {
		t.Fatalf("trusted legacy credential file returned value=%q error=%v", value, err)
	}
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(path, owner) || !credentialFilePrivate(path) {
		t.Fatal("trusted legacy credential file was not migrated to the current owner and protected ACL")
	}
}

func TestWindowsCredentialChildCreatedWithNetworkTokenDefaultsIsSecured(t *testing.T) {
	directory, sddl := preparePrivateWindowsCredentialDirectory(t)
	path := filepath.Join(directory, "profiles")
	created := false
	err := ensureDPAPIDirectoryWithCreate(path, sddl, func(parent windows.Handle, path, sddl string) (windows.Handle, windows.ByHandleFileInformation, string, error) {
		created = true
		handle, information, finalPath, err := createDPAPIDirectoryHandle(parent, path, sddl)
		if err != nil {
			return 0, windows.ByHandleFileInformation{}, "", err
		}
		makeWindowsNetworkTokenDirectoryFixture(t, path, false)
		return handle, information, finalPath, nil
	})
	if err != nil {
		t.Fatalf("secure newly created network-token directory: %v", err)
	}
	if !created {
		t.Fatal("network-token creation fixture was not used")
	}
	if !credentialFilePrivate(path) {
		t.Fatal("new network-token directory was not secured to the current user")
	}
}

func TestWindowsCredentialLegacyNetworkDirectoriesMigrateUnderPrivateParent(t *testing.T) {
	for _, name := range []string{"pending-revocations", "transactions"} {
		t.Run(name, func(t *testing.T) {
			directory, sddl := preparePrivateWindowsCredentialDirectory(t)
			path := filepath.Join(directory, name)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			makeWindowsNetworkTokenDirectoryFixture(t, path, false)
			if err := ensureDPAPIDirectory(path, sddl); err != nil {
				t.Fatalf("migrate %s network-token directory: %v", name, err)
			}
			if !credentialFilePrivate(path) {
				t.Fatalf("%s network-token directory was not migrated to the current user", name)
			}
		})
	}
}

func TestWindowsCredentialLegacyNetworkDirectoryRejectsExtraACE(t *testing.T) {
	directory, sddl := preparePrivateWindowsCredentialDirectory(t)
	path := filepath.Join(directory, "pending-revocations")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	administrators := makeWindowsNetworkTokenDirectoryFixture(t, path, true)
	if err := ensureDPAPIDirectory(path, sddl); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("network-token directory with extra ACE error=%v", err)
	}
	if !windowssecurity.OwnerMatchesSID(path, administrators) {
		t.Fatal("rejected network-token directory owner was changed")
	}
}

func TestWindowsCredentialLegacyNetworkDirectoryDoesNotBroadenRootMigration(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)
	path := filepath.Join(localAppData, "Paperboat")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	administrators := makeWindowsNetworkTokenDirectoryFixture(t, path, false)
	sddl, err := currentUserCredentialSDDL()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureDPAPIDirectory(path, sddl); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("network-token Paperboat root error=%v", err)
	}
	if !windowssecurity.OwnerMatchesSID(path, administrators) {
		t.Fatal("rejected network-token Paperboat root owner was changed")
	}
}

func TestWindowsMachineScopeCredentialRejectsAdministratorsOwnedFileWithExtraACE(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	store := KeyringStore{}
	ref := fmt.Sprintf("native-hostile-legacy-file-owner-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = store.Delete(ref) })
	if err := store.Set(ref, "machine-secret"); err != nil {
		t.Fatal(err)
	}
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	administrators := changeFixtureOwnerToAdministrators(t, path)
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + owner.String() + ")(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get(ref); value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("Administrators-owned credential with extra ACE returned value=%q error=%v", value, err)
	}
	if !windowssecurity.OwnerMatchesSID(path, administrators) {
		t.Fatal("rejected credential file owner was mutated")
	}
}

func TestWindowsMachineScopeSetDoesNotMutateForeignOwnedDirectory(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)
	directory := filepath.Join(localAppData, "Paperboat", "credentials")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignOwner := changeFixtureOwnerToAdministrators(t, directory)
	ref := fmt.Sprintf("native-foreign-precreate-%d", time.Now().UnixNano())
	if err := (KeyringStore{}).Set(ref, "machine-secret"); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("foreign-owned precreated directory Set error=%v", err)
	}
	if !windowssecurity.OwnerMatchesSID(directory, foreignOwner) {
		t.Fatal("Set changed the foreign-owned credential directory owner")
	}
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Set created a credential in foreign-owned state: %v", err)
	}
}

func TestWindowsMachineScopeSetDoesNotMutateForeignOwnedRoot(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)
	root := filepath.Join(localAppData, "Paperboat")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignOwner := changeFixtureOwnerToAdministrators(t, root)
	// An Administrators-owned root whose ACL holds exactly SYSTEM, Administrators and the user is the trusted legacy
	// layout that Set migrates, and a temporary directory can inherit exactly that. A foreign root admits someone else.
	owner, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + owner.String() + ")(A;OICI;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("native-foreign-root-%d", time.Now().UnixNano())
	if err := (KeyringStore{}).Set(ref, "machine-secret"); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("foreign-owned precreated root Set error=%v", err)
	}
	if !windowssecurity.OwnerMatchesSID(root, foreignOwner) {
		t.Fatal("Set changed the foreign-owned credential root owner")
	}
	_, directory, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Set created a directory below foreign-owned state: %v", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("must-not-be-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := (KeyringStore{}).Get(ref); value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("foreign-owned root Get returned value=%q error=%v", value, err)
	}
	if !windowssecurity.OwnerMatchesSID(root, foreignOwner) {
		t.Fatal("Get changed the foreign-owned credential root owner")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "must-not-be-read" {
		t.Fatalf("Get mutated the foreign-owned credential: body=%q error=%v", body, err)
	}
}

func TestWindowsMachineScopeGetRejectsReparseRoot(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)
	target := t.TempDir()
	targetRoot := filepath.Join(target, "Paperboat")
	directory := filepath.Join(targetRoot, "credentials")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(localAppData, "Paperboat")
	if err := os.Symlink(targetRoot, root); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skipf("creating the hostile reparse fixture requires an elevated native runner: %v", err)
		}
		t.Fatal(err)
	}
	ref := fmt.Sprintf("native-reparse-root-%d", time.Now().UnixNano())
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("must-not-be-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := (KeyringStore{}).Get(ref); value != "" || !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("reparse-root credential returned value=%q error=%v", value, err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "must-not-be-read" {
		t.Fatalf("reparse-root fixture was mutated: body=%q error=%v", body, err)
	}
}

func TestWindowsDPAPIAuthorityRejectsEmptyCredential(t *testing.T) {
	ref := fmt.Sprintf("native-empty-%d", time.Now().UnixNano())
	store := KeyringStore{}
	if err := store.Set(ref, ""); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("empty credential error = %v", err)
	}
	if _, err := store.Get(ref); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("empty credential was persisted: %v", err)
	}
}

func TestWindowsPasswordVaultDPAPIBounds(t *testing.T) {
	ref := passwordVaultSecretRef("https://vault.example", fmt.Sprintf("native-%d", time.Now().UnixNano()))
	store := KeyringStore{}
	t.Cleanup(func() { cleanupWindowsKeyringFixture(ref) })
	value := string(bytes.Repeat([]byte("v"), passwordVaultRecordBytes))
	if err := store.Set(ref, value); err != nil {
		t.Fatalf("store maximum vault record: %v", err)
	}
	actual, err := store.Get(ref)
	if err != nil || actual != value {
		t.Fatalf("maximum vault round trip failed: bytes=%d err=%v", len(actual), err)
	}
	if err := store.Set(ref, value+"x"); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("oversize vault accepted: %v", err)
	}
	actual, err = store.Get(ref)
	if err != nil || actual != value {
		t.Fatalf("rejected write changed authoritative record: %v", err)
	}
	for _, other := range []string{"ordinary-auth", ref + "x", "environment-password-vault-v1-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if windowsSecretMaxBytes(other) != windowsCredentialBlobMaxBytes {
			t.Fatal("noncanonical reference received expanded allowance")
		}
		if err := store.Set(other, string(bytes.Repeat([]byte("a"), windowsCredentialBlobMaxBytes+1))); !errors.Is(err, ErrCredentialStoreUnavailable) {
			t.Fatalf("oversize ordinary credential accepted: %v", err)
		}
	}
	protected, err := protectKeyringDPAPIV2(ref, value)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(protected)
	otherRef := passwordVaultSecretRef("https://vault.example", "other")
	if _, err := unprotectKeyringDPAPIV2(otherRef, protected); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("cross-reference vault accepted: %v", err)
	}
	path, _, err := dpapiSecretPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, keyringDPAPIMaxBytes(ref)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ref); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("oversize protected record accepted: %v", err)
	}
}
