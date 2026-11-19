package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/tc-hib/winres"
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
