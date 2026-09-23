package workerupdate

import (
	"io/fs"
	"strings"
)

const (
	maxPackageManualCount = 1024
	maxPackageManualBytes = 128 << 10
)

// The package boundary permits future command names without granting arbitrary
// payload paths or relying on the updating binary's older command inventory.
func validPackageManualPath(path string) bool {
	const prefix = "usr/local/share/man/man1/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".1") {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".1")
	if name == "pb" {
		return true
	}
	if !strings.HasPrefix(name, "pb-") || len(name) == 3 {
		return false
	}
	for _, c := range name[3:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validPackageManual(path string, info fs.FileInfo) bool {
	return validPackageManualPath(path) && info.Mode().IsRegular() && info.Size() >= 0 && info.Size() <= maxPackageManualBytes
}
