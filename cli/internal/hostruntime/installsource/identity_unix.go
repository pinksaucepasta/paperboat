//go:build linux || darwin

package installsource

import (
	"os"
	"syscall"
	"time"
)

// fileIdentity is a regular file's exact on-disk identity; any change to its bytes or metadata
// through the file system changes it.
type fileIdentity struct {
	device, inode    uint64
	size             int64
	mtimeSeconds     int64
	mtimeNanoseconds int64
	ctimeSeconds     int64
	ctimeNanoseconds int64
}

func fileIdentityOf(path string) (fileIdentity, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fileIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return statIdentity(stat), true
}

// changed is the file's last data or metadata change.
func (i fileIdentity) changed() time.Time {
	m, c := time.Unix(i.mtimeSeconds, i.mtimeNanoseconds), time.Unix(i.ctimeSeconds, i.ctimeNanoseconds)
	if m.After(c) {
		return m
	}
	return c
}
