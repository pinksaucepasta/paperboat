//go:build windows

package hostinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var task23PinGCOwner = flag.String("task23-pin-gc-owner", "", "enrolled Windows SID for isolated privileged pin custody qualification")

// This exercises real Windows handles, ACLs, links, and deletion. It does not
// replace the live owner/SSH update scenario that qualifies reference capture.
func TestNativeWindowsPinGCCustody(t *testing.T) {
	sid := *task23PinGCOwner
	if sid == "" {
		t.Skip("requires privileged Windows fixture and enrolled SID")
	}
	if !validSID(sid) {
		t.Fatal("invalid enrolled SID")
	}
	allowed, err := checkAdministrator()
	if err != nil || !allowed {
		t.Fatalf("privileged fixture required: %v", err)
	}
	fixture, err := os.MkdirTemp(WindowsProgramDataRoot(), "task23-pin-gc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fixture)
	releases := filepath.Join(fixture, "releases")
	pins := filepath.Join(releases, "versions")
	for _, path := range []string{releases, pins} {
		if err := protectWindowsPinDirectory(path, sid); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := readInstallFile(exe, 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	write := func(label string, writable bool) string {
		t.Helper()
		body := append(append([]byte(nil), source...), []byte(label)...)
		sum := sha256.Sum256(body)
		path := filepath.Join(pins, "custom-"+hex.EncodeToString(sum[:]), "pb.exe")
		if err := protectWindowsPinDirectory(filepath.Dir(path), sid); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		dacl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + sid + ")"
		if writable {
			dacl = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")"
		}
		if err := applyWindowsOwnedDACL(path, owner, dacl); err != nil {
			t.Fatal(err)
		}
		return path
	}
	live := write("live", false)
	retired := write("retired", false)
	writable := write("writable", true)
	neighbor := write("neighbor", false)
	if err := os.WriteFile(filepath.Join(filepath.Dir(neighbor), "notes.txt"), []byte("owned fixture neighbor"), 0600); err != nil {
		t.Fatal(err)
	}
	wrong := write("wrong-digest", false)
	wrongDir := filepath.Join(pins, "custom-"+hex.EncodeToString(make([]byte, 32)))
	if err := os.Rename(filepath.Dir(wrong), wrongDir); err != nil {
		t.Fatal(err)
	}
	wrong = filepath.Join(wrongDir, "pb.exe")
	outside := write("outside-link-target", false)
	aliasName := filepath.Base(filepath.Dir(outside))
	outsideDir := filepath.Join(fixture, "outside")
	if err := os.Rename(filepath.Dir(outside), outsideDir); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(outsideDir, "pb.exe")
	alias := filepath.Join(pins, aliasName)
	if err := os.Symlink(outsideDir, alias); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(alias)
	hardlink := write("hard-link-target", false)
	hardlinkTarget := filepath.Join(fixture, "hardlink-target.exe")
	if err := os.Link(hardlink, hardlinkTarget); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{strings.ToLower(live): true}
	if err := pruneProtectedWindowsPins(context.Background(), pins, sid, keep, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(retired); !os.IsNotExist(err) {
		t.Fatalf("unreferenced verified pin remained: %v", err)
	}
	for _, path := range []string{live, writable, neighbor, wrong, outside, alias, hardlink, hardlinkTarget} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("retained/unknown custody changed: %v", err)
		}
	}
	// An unsafe parent must reject the entire sweep before touching even a
	// valid, unreferenced pin inside it.
	unsafe := write("unsafe-parent", false)
	if err := applyWindowsOwnedDACL(pins, owner, windowsRuntimeLegacyRootDACL(sid)); err != nil {
		t.Fatal(err)
	}
	if err := pruneProtectedWindowsPins(context.Background(), pins, sid, nil, 32); err == nil {
		t.Fatal("writable parent accepted for pin deletion")
	}
	if _, err := os.Lstat(unsafe); err != nil {
		t.Fatalf("unsafe-parent pin changed: %v", err)
	}
}
