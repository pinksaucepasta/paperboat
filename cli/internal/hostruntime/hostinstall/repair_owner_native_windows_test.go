//go:build windows

package hostinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"golang.org/x/sys/windows"
)

// Runs elevated (elevated_tests.txt), as `pb install` does: the tree gets the enrolled owner (here this account)
// and its DACL, and its content is untouched.
func TestNativeRepairWindowsTreeOwnerElevated(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := tokenUser.User.Sid.String()
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	if err := os.WriteFile(path, []byte("retained state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repairWindowsTreeACL(root, ownerSID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, path} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner.String() != ownerSID {
			t.Fatalf("%s: enrolled owner not assigned", path)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "retained state" {
		t.Fatal("state content changed")
	}
}

// A hard link planted in the tree names a file outside it: the repair refuses it instead of handing that file to
// the enrolled owner.
func TestNativeRepairWindowsTreeRefusesAHardLinkToAnOutsideFile(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Link(outside, filepath.Join(root, "planted")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := repairWindowsTreeACL(root, tokenUser.User.Sid.String()); err == nil {
		t.Fatal("repair accepted a hard link to a file outside the tree")
	}
	after, err := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatalf("the outside file's DACL changed: %s -> %s", before, after)
	}
}

// A file received through the transfer service sits in the user-state tree (the spool) and in the Inbox: the repair that
// `pb install` runs over the tree must still succeed (a second hard link in the tree made it refuse every update).
func TestNativeRepairWindowsTreeAcceptsAReceivedFile(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	stateRoot, inbox := t.TempDir(), t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	service, err := filetransfer.New(filetransfer.Config{Root: filepath.Join(stateRoot, "file-transfers"), PublishRoot: func() (string, error) { return inbox, nil }, EnsurePublishRoot: func(string) error { return nil }, LocalMachineID: "machine_local", Store: durable})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("received through pb send")
	sum := sha256.Sum256(data)
	created, err := service.Create(context.Background(), filetransfer.CreateRequest{BatchID: "fb_1", SourceMachineID: "machine_source", DestinationMachineID: "machine_local", InitiatingUserID: "user_1", SessionID: "ses_1", Files: []filetransfer.File{{Basename: "received.txt", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	if _, err := service.Append(context.Background(), id, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishedPath(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := repairWindowsTreeACL(stateRoot, tokenUser.User.Sid.String()); err != nil {
		t.Fatalf("the user-state repair refused a tree holding a received file: %v", err)
	}
	if body, err := os.ReadFile(published); err != nil || !bytes.Equal(body, data) {
		t.Fatalf("the received file changed: %q %v", body, err)
	}
}
