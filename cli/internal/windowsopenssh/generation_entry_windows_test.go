//go:build windows

package windowsopenssh

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// The native reader owns startup authorization independently of the later
// runtime-install.json producer. This fixture never registers or starts SCM.
func TestSSHGenerationStartupBeforeRuntimeCommit(t *testing.T) {
	owner := fmt.Sprintf("S-1-5-21-52157-52158-52159-%d", uint32(time.Now().UnixNano()))
	layout, err := service.WindowsUserLayout(owner)
	if err != nil {
		t.Fatal(err)
	}
	baseLayout, err := service.DefaultLayout("windows")
	if err != nil {
		t.Fatal(err)
	}
	accountRoot := filepath.Join(filepath.Dir(baseLayout.UpdateStateRoot), "users", layout.Instance)
	if _, err := os.Lstat(accountRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("isolated fixture already exists")
	}
	if _, err := os.Lstat(layout.InstallRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("isolated pin fixture already exists")
	}
	t.Cleanup(func() { _ = os.RemoveAll(accountRoot); _ = os.RemoveAll(layout.InstallRoot) })
	secureDirectory := func(path string) {
		t.Helper()
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		sd, err := windows.SecurityDescriptorFromString("O:SY" + generationDirectoryDACL(owner))
		if err != nil {
			t.Fatal(err)
		}
		sid, _, err := sd.Owner()
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sid, nil, dacl, nil)
		}); err != nil {
			t.Fatal(err)
		}
	}
	secureWrite := func(path string, body []byte, acl string) {
		t.Helper()
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return atomicfile.Write(path, body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SY" + acl})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A small, never-executed PE target fixture verifies hash and target binding.
	pe := make([]byte, 154)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[60:64], 128)
	copy(pe[128:], "PE\x00\x00")
	machine := uint16(0x8664)
	if runtime.GOARCH == "arm64" {
		machine = 0xaa64
	}
	binary.LittleEndian.PutUint16(pe[132:134], machine)
	binary.LittleEndian.PutUint16(pe[148:150], 2)
	binary.LittleEndian.PutUint16(pe[152:154], 0x20b)
	hash := sha256.Sum256(pe)
	source := installsource.Source{Version: "native-reader-fixture", Platform: "windows", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(hash[:]), Length: int64(len(pe)), Distribution: installsource.Custom}
	pinRoot := filepath.Join(layout.ReleasesRoot, "versions", "custom-"+source.SHA256)
	for _, path := range []string{layout.InstallRoot, layout.ReleasesRoot, filepath.Join(layout.ReleasesRoot, "versions"), pinRoot} {
		secureDirectory(path)
	}
	executable := filepath.Join(pinRoot, "pb.exe")
	secureWrite(executable, pe, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;"+owner+")")
	generation := source.SHA256[:32]
	_, root, err := generationPaths(owner, generation)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{accountRoot, filepath.Join(accountRoot, "ssh"), filepath.Join(accountRoot, "ssh", "generations"), root} {
		secureDirectory(path)
	}
	secureWrite(filepath.Join(root, "sshd_config"), []byte("test-owned configuration"), generationFileDACL(owner))
	d := GenerationDescriptor{Schema: sshGenerationSchema, Instance: layout.Instance, Generation: generation, OwnerSID: owner, Source: source, Executable: executable, Port: 45000}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "descriptor.json")
	secureWrite(path, body, generationFileDACL(owner))
	if _, err := os.Lstat(filepath.Join(accountRoot, "runtime-install.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("future runtime metadata unexpectedly present")
	}
	got, err := LoadGenerationForInstance(context.Background(), layout.Instance, generation)
	if err != nil || got != d {
		t.Fatalf("protected generation cannot start before runtime commit: %T", err)
	}
	for _, tc := range []struct {
		name string
		body []byte
		acl  string
	}{
		{"malformed", []byte("{}"), generationFileDACL(owner)},
		{"mutable", body, generationFileDACL(owner) + "(A;;FW;;;BU)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secureWrite(path, tc.body, tc.acl)
			if _, err := LoadGenerationForInstance(context.Background(), layout.Instance, generation); err == nil {
				t.Fatal("unsafe descriptor granted startup authority")
			}
		})
	}
	foreign := d
	foreign.OwnerSID = "S-1-5-21-52157-52158-52159-1"
	foreignBody, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	secureWrite(path, foreignBody, generationFileDACL(owner))
	if _, err := LoadGenerationForInstance(context.Background(), layout.Instance, generation); err == nil {
		t.Fatal("foreign descriptor granted startup authority")
	}
}
