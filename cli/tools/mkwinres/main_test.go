package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func requireBranded(t *testing.T, name string, object []byte) {
	t.Helper()
	for _, want := range []string{"FileDescription", "ProductName", "CompanyName", "Paperboat", "pb.exe", "longPathAware"} {
		if !bytes.Contains(object, utf16le(want)) && !bytes.Contains(object, []byte(want)) {
			t.Errorf("%s: resource object lacks %q", name, want)
		}
	}
}

func TestGeneratedObjectNamesPaperboat(t *testing.T) {
	for arch, a := range map[string]winres.Arch{"amd64": winres.ArchAMD64, "arm64": winres.ArchARM64} {
		var b bytes.Buffer
		if err := write(&b, "2026.11.09.7", a); err != nil {
			t.Fatal(err)
		}
		requireBranded(t, arch, b.Bytes())
		if !bytes.Contains(b.Bytes(), utf16le("2026.11.09.7")) {
			t.Errorf("%s: release version missing", arch)
		}
	}
}

func TestDevVersionKeepsNumericFileVersion(t *testing.T) {
	var b bytes.Buffer
	if err := write(&b, "dev-abc123", winres.ArchAMD64); err != nil {
		t.Fatal(err)
	}
	requireBranded(t, "dev", b.Bytes())
}

// The committed objects are what plain `go build` links; they must carry the same branding.
func TestCommittedObjectsNamePaperboat(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		name := filepath.Join("..", "..", "cmd", "pb", "windows_manifest_windows_"+arch+".syso")
		object, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		requireBranded(t, name, object)
	}
}

// A plain `go build` of pb for Windows must link the Paperboat version resource (what dialogs read).
func TestBuiltWindowsExecutableNamesPaperboat(t *testing.T) {
	if testing.Short() {
		t.Skip("builds cmd/pb for two Windows architectures")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		exe := filepath.Join(t.TempDir(), "pb.exe")
		build := exec.Command("go", "build", "-o", exe, "./cmd/pb")
		build.Dir = filepath.Join("..", "..")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH="+arch)
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("%s: build: %v\n%s", arch, err, out)
		}
		f, err := os.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		rs, err := winres.LoadFromEXE(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: read resources: %v", arch, err)
		}
		raw := rs.Get(winres.RT_VERSION, winres.ID(1), winres.LCIDDefault)
		if raw == nil {
			t.Fatalf("%s: pb.exe has no version resource", arch)
		}
		vi, err := version.FromBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		table := vi.Table()[version.LangDefault]
		for key, want := range map[string]string{version.FileDescription: "Paperboat", version.ProductName: "Paperboat", version.CompanyName: "Paperboat", version.OriginalFilename: "pb.exe"} {
			if got := (*table)[key]; got != want {
				t.Errorf("%s: %s=%q want %q", arch, key, got, want)
			}
		}
		if rs.Get(winres.RT_MANIFEST, winres.ID(1), winres.LCIDDefault) == nil {
			t.Errorf("%s: pb.exe lost its manifest", arch)
		}
	}
}
