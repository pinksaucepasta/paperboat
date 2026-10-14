//go:build windows

package configsync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// This test prints stage names and filesystem security metadata only, never
// profile contents, credentials, private keys or credential-reference contents.
func TestWindowsRepositoryCredentialSaveLoadProtectedReferences(t *testing.T) {
	root := os.Getenv("PB_CREDENTIAL_TEST_ROOT")
	if root == "" {
		root = filepath.Join(t.TempDir(), "credentials")
	}
	if !canonicalAbsolutePath(root) {
		t.Fatal("root stage: non-native canonical root")
	}
	for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(parent)
			t.Logf("existing parent stage: exact_spelling=%t trusted_resolution=%t resolve_err=%v", resolved == parent, repositoryReferencePathMatches(parent, resolved), resolveErr)
			break
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		resolved, resolveErr := filepath.EvalSymlinks(root)
		info, statErr := os.Lstat(root)
		token, tokenErr := windows.OpenCurrentProcessToken()
		ownerMatches := false
		if tokenErr == nil {
			defer token.Close()
			user, userErr := token.GetTokenUser()
			descriptor, aclErr := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
			if userErr == nil && aclErr == nil {
				owner, _, ownerErr := descriptor.Owner()
				ownerMatches = ownerErr == nil && owner != nil && owner.Equals(user.User.Sid)
			}
		}
		t.Fatalf("ensure root stage: err=%v resolved_same=%t resolve_err=%v stat_err=%v private_acl=%t owner_matches_token=%t token_err=%v", err, resolved == root, resolveErr, statErr, privateRepositoryCredentialDirectory(root, info), ownerMatches, tokenErr)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || !repositoryReferencePathMatches(root, resolved) {
		t.Fatalf("resolved root stage: same_spelling=%t error=%v", resolved == root, err)
	}
	info, err := os.Lstat(root)
	if err != nil || !privateRepositoryCredentialDirectory(root, info) {
		t.Fatalf("private root stage: %v", err)
	}
	referenceRoot := filepath.Join(t.TempDir(), "references")
	if err := EnsureRepositoryCredentialRoot(referenceRoot); err != nil {
		t.Fatal("ensure reference directory stage", err)
	}
	key := filepath.Join(referenceRoot, "id_ed25519")
	hosts := filepath.Join(referenceRoot, "known_hosts")
	for _, path := range []string{key, hosts} {
		if err := writePrivateAtomic(path, []byte("test-only reference\n")); err != nil {
			t.Fatal("private reference creation stage", err)
		}
		if _, err := readRepositoryReference(path, true); err != nil {
			t.Fatal("private reference read stage", err)
		}
	}
	profile := RepositoryCredentialProfile{Transport: "ssh", Auth: "ssh", Username: "git", SSHKeyFile: key, KnownHostsFile: hosts}
	if !validRepositoryProfile(profile) {
		t.Fatal("profile validation stage")
	}
	id := "windows-private-profile-regression-" + time.Now().Format("20060102150405.000000000")
	path, err := credentialProfilePath(root, id)
	if err != nil {
		t.Fatal("profile path stage", err)
	}
	defer os.Remove(path)
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal("current token owner stage", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal("current token owner stage", err)
	}
	ownerDescriptor, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal("root actual owner stage", err)
	}
	owner, _, err := ownerDescriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("root actual owner is not current user")
	}
	if err := SaveRepositoryCredentialProfile(root, id, profile); err != nil {
		t.Fatal("save stage", err)
	}
	loaded, err := LoadRepositoryCredentialProfile(root, id)
	if err != nil {
		t.Fatal("load stage", err)
	}
	if loaded != profile {
		t.Fatal("round-trip stage: profile differed")
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal("profile ACL stage", err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("profile ACL stage: inheritance enabled")
	}
	// A broadly writable reference must fail even though Windows reports the
	// same POSIX-looking permission bits for both secure and insecure files.
	bad, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := bad.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(key, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal("untrusted reference ACL setup stage", err)
	}
	if _, err := readRepositoryReference(key, true); err == nil {
		t.Fatal("untrusted writable reference accepted")
	}
	if err := DeleteRepositoryCredentialProfile(root, id); err != nil {
		t.Fatal("delete stage", err)
	}
}

func TestWindowsRepositoryCredentialCaseOnlySameObject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "CaseSensitiveSpelling")
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(filepath.Dir(root), "casesensitivespelling")
	if !repositoryReferencePathMatches(lower, root) {
		t.Fatal("casing-only same filesystem object rejected")
	}
	if err := EnsureRepositoryCredentialRoot(lower); err != nil {
		t.Fatal("casing-only root rejected", err)
	}
	if err := SaveRepositoryCredentialProfile(lower, "case-test", RepositoryCredentialProfile{Transport: "local", Auth: "anonymous"}); err != nil {
		t.Fatal("case root save", err)
	}
	if _, err := LoadRepositoryCredentialProfile(lower, "case-test"); err != nil {
		t.Fatal("case root load", err)
	}
	different := filepath.Join(t.TempDir(), "CaseSensitiveSpelling")
	os.Mkdir(different, 0700)
	if repositoryReferencePathMatches(root, different) {
		t.Fatal("different filesystem object accepted")
	}
}

func TestWindowsRepositoryCredentialExistingForeignOwnerRejected(t *testing.T) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	if !token.IsElevated() {
		t.Skip("foreign-owner setup requires elevated token")
	}
	root := filepath.Join(t.TempDir(), "foreign-existing")
	// os.Mkdir deliberately uses the elevated token's default Administrators
	// owner, rather than the explicit current-user creation used in production.
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	user, userErr := token.GetTokenUser()
	if err != nil || userErr != nil {
		t.Fatal("owner fixture inspection failed")
	}
	if owner.Equals(user.User.Sid) {
		t.Skip("token defaults new directories to current user")
	}
	if err := EnsureRepositoryCredentialRoot(root); err == nil {
		t.Fatal("existing foreign owner accepted")
	}
}

func TestWindowsRepositoryCredentialPrivateStagingCurrentOwner(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private-parent")
	if err := EnsureRepositoryCredentialRoot(parent); err != nil {
		t.Fatal(err)
	}
	staging, err := newPrivateRepositoryTemporaryDirectory(parent, ".repository-read-")
	if err != nil {
		t.Fatal("private staging creation", err)
	}
	defer os.RemoveAll(staging)
	if err := EnsureRepositoryCredentialRoot(staging); err != nil {
		t.Fatal("trusted staging was rejected", err)
	}
	info, err := os.Lstat(staging)
	if err != nil || !privateRepositoryCredentialDirectory(staging, info) {
		t.Fatal("staging is not current-owner private")
	}
}
