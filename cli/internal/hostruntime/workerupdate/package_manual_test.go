package workerupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackageManualPathBoundary(t *testing.T) {
	for _, name := range []string{"pb.1", "pb-connect.1", "pb-next-command-123.1"} {
		if !validPackageManualPath("usr/local/share/man/man1/" + name) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, path := range []string{"usr/local/share/man/man1/pb-.1", "usr/local/share/man/man1/pb-X.1", "usr/local/share/man/man1/pb-é.1", "usr/local/share/man/man1/pb-a_b.1", "usr/local/share/man/man1/pb-a/b.1", "usr/local/share/man/man1/pb-../evil.1", "usr/local/share/man/man1/other.1", "usr/local/share/man/man2/pb.1", "/usr/local/share/man/man1/pb.1", "usr/local/share/man/man1/pb.1.gz"} {
		if validPackageManualPath(path) {
			t.Errorf("accepted %q", path)
		}
	}
}

func TestPackageManualFileBoundary(t *testing.T) {
	const name = "usr/local/share/man/man1/pb.1"
	root := t.TempDir()
	path := filepath.Join(root, "pb.1")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, size := range []int64{0, maxPackageManualBytes, maxPackageManualBytes + 1} {
		if err := file.Truncate(size); err != nil {
			t.Fatal(err)
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if got := validPackageManual(name, info); got != (size <= maxPackageManualBytes) {
			t.Errorf("size %d accepted=%v", size, got)
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if validPackageManual(name, info) {
		t.Fatal("accepted directory")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if validPackageManual(name, info) {
		t.Fatal("accepted symlink")
	}
}
