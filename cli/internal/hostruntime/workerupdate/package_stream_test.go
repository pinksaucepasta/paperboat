package workerupdate

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func packageTar(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for _, h := range headers {
		if err := writer.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 1024 {
			return body.Bytes()
		} // Rejection must precede body reads.
		if h.Size > 0 {
			if _, err := writer.Write(make([]byte, h.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestPackagePayloadStreamBoundsAndPaths(t *testing.T) {
	helper := "Library/PrivilegedHelperTools/Paperboat/bin/pb"
	cli := "usr/local/bin/pb"
	good := []*tar.Header{{Name: helper, Typeflag: tar.TypeReg, Mode: 0755, Size: 3}, {Name: cli, Typeflag: tar.TypeSymlink, Linkname: darwinPackageCLITarget}}
	cases := []struct {
		name   string
		extra  *tar.Header
		reject bool
	}{
		{"valid", nil, false},
		{"manual", &tar.Header{Name: "usr/local/share/man/man1/pb.1", Typeflag: tar.TypeReg, Size: 2}, false},
		{"duplicate", good[0], true},
		{"traversal", &tar.Header{Name: "../outside", Typeflag: tar.TypeReg}, true},
		{"unexpected", &tar.Header{Name: "unexpected", Typeflag: tar.TypeReg}, true},
		{"manual bomb", &tar.Header{Name: "usr/local/share/man/man1/pb.1", Typeflag: tar.TypeReg, Size: maxPackageManualBytes + 1}, true},
		{"hardlink", &tar.Header{Name: "usr/local/share/man/man1/pb.1", Typeflag: tar.TypeLink, Linkname: helper}, true},
		{"manual symlink", &tar.Header{Name: "usr/local/share/man/man1/pb.1", Typeflag: tar.TypeSymlink, Linkname: "/tmp/outside"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			headers := append([]*tar.Header{}, good...)
			if tc.extra != nil {
				headers = append(headers, tc.extra)
			}
			err := extractDarwinPayloadStream(bytes.NewReader(packageTar(t, headers)), root)
			if (err != nil) != tc.reject {
				t.Fatalf("error=%v reject=%v", err, tc.reject)
			}
			if !tc.reject {
				data, err := os.ReadFile(filepath.Join(root, helper))
				if err != nil || len(data) != 3 {
					t.Fatalf("payload=%d error=%v", len(data), err)
				}
				target, err := os.Readlink(filepath.Join(root, cli))
				if err != nil || target != darwinPackageCLITarget {
					t.Fatalf("link=%q error=%v", target, err)
				}
			}
			if tc.name == "manual bomb" {
				if _, err := os.Lstat(filepath.Join(root, tc.extra.Name)); !os.IsNotExist(err) {
					t.Fatalf("oversized file created: %v", err)
				}
			}
		})
	}
	t.Run("manual count", func(t *testing.T) {
		headers := append([]*tar.Header{}, good...)
		for i := 0; i <= maxPackageManualCount; i++ {
			headers = append(headers, &tar.Header{Name: fmt.Sprintf("usr/local/share/man/man1/pb-command-%d.1", i), Typeflag: tar.TypeReg})
		}
		root := t.TempDir()
		if err := extractDarwinPayloadStream(bytes.NewReader(packageTar(t, headers)), root); err == nil {
			t.Fatal("too many manuals accepted")
		}
		if _, err := os.Lstat(filepath.Join(root, headers[len(headers)-1].Name)); !os.IsNotExist(err) {
			t.Fatalf("excess manual created: %v", err)
		}
	})
	for _, h := range []*tar.Header{
		{Name: helper, Typeflag: tar.TypeReg, Mode: 0755, Size: maxRuntimeBytes + 1},
		{Name: cli, Typeflag: tar.TypeSymlink, Linkname: "/tmp/outside"},
	} {
		root := t.TempDir()
		if err := extractDarwinPayloadStream(bytes.NewReader(packageTar(t, []*tar.Header{h})), root); err == nil {
			t.Fatal("unsafe required entry accepted")
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatal("unsafe required entry wrote to disk")
		}
	}
}

func TestPackageOuterStreamBoundsAndInventory(t *testing.T) {
	good := []*tar.Header{{Name: "Bom", Typeflag: tar.TypeReg, Size: 1}, {Name: "PackageInfo", Typeflag: tar.TypeReg, Size: 1}, {Name: "Payload", Typeflag: tar.TypeReg, Size: 1}}
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		reject  bool
	}{
		{"valid", good, false},
		{"missing", good[:2], true},
		{"duplicate", append(append([]*tar.Header{}, good...), good[0]), true},
		{"scripts", append(append([]*tar.Header{}, good...), &tar.Header{Name: "Scripts", Typeflag: tar.TypeDir}), true},
		{"BOM bomb", []*tar.Header{{Name: "Bom", Typeflag: tar.TypeReg, Size: maxPackageBOMBytes + 1}}, true},
		{"XML bomb", []*tar.Header{{Name: "PackageInfo", Typeflag: tar.TypeReg, Size: maxPackageInfoBytes + 1}}, true},
		{"payload bomb", []*tar.Header{{Name: "Payload", Typeflag: tar.TypeReg, Size: maxRuntimeBytes + 1}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			err := extractDarwinOuterStream(bytes.NewReader(packageTar(t, tc.headers)), root)
			if (err != nil) != tc.reject {
				t.Fatalf("error=%v reject=%v", err, tc.reject)
			}
			if len(tc.headers) == 1 {
				entries, _ := os.ReadDir(root)
				if len(entries) != 0 {
					t.Fatal("oversized member created")
				}
			}
		})
	}
}
