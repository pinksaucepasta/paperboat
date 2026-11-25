// Package dnsfile keeps shared/dnscap's verified addresses between processes: each pb is a new one.
package dnsfile

import (
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/shared/dnscap"
)

// Open loads path and writes every later verified address to it, atomically and owner-only. A link, or a file another
// account owns or can touch, is ignored; a missing directory keeps the addresses in memory.
func Open(path string) {
	if !filepath.IsAbs(path) {
		return
	}
	var body []byte
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() <= 16<<10 && trusted(path, info) {
		body, _ = os.ReadFile(path)
	}
	dnscap.Persist(body, func(body []byte) {
		_ = atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
	})
}
