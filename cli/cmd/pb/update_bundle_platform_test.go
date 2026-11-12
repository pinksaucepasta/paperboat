package main

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleMadeForOtherOSReadsTheExecutableMagic(t *testing.T) {
	write := func(artifact []byte) string {
		path := filepath.Join(t.TempDir(), "x.pb-update")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		archive := tar.NewWriter(file)
		for name, body := range map[string][]byte{"metadata/root.json": []byte("{}"), "artifact": artifact} {
			if name == "artifact" {
				continue
			}
			archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body)), Typeflag: tar.TypeReg})
			archive.Write(body)
		}
		archive.WriteHeader(&tar.Header{Name: "artifact", Mode: 0600, Size: int64(len(artifact)), Typeflag: tar.TypeReg})
		archive.Write(artifact)
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	elf, pe, macho := write([]byte("\x7fELF....")), write([]byte("MZ......")), write([]byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0})
	for _, tc := range []struct {
		path, current, made string
		wrong               bool
	}{
		{elf, "windows", "linux", true}, {elf, "linux", "linux", false}, {pe, "darwin", "windows", true}, {pe, "windows", "windows", false},
		{macho, "linux", "darwin", true}, {macho, "darwin", "darwin", false}, {write([]byte("????....")), "linux", "", false},
	} {
		if made, wrong := bundleMadeForOtherOS(tc.path, tc.current); made != tc.made || wrong != tc.wrong {
			t.Errorf("bundle for %q on %s: got (%q,%v), want (%q,%v)", tc.made, tc.current, made, wrong, tc.made, tc.wrong)
		}
	}
	if made, wrong := bundleMadeForOtherOS(filepath.Join(t.TempDir(), "missing"), "linux"); made != "" || wrong {
		t.Errorf("a missing bundle must be left to the signature checks, got (%q,%v)", made, wrong)
	}
}
