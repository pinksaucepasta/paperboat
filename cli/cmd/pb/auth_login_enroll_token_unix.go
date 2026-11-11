//go:build !windows

package main

import "github.com/pinksaucepasta/paperboat/internal/atomicfile"

// writeLoginEnrollmentToken writes a new owner-only file atomically; a symlink or other non-regular file planted at
// path is refused, never followed (the Windows build writes the same way with an owner-only descriptor).
func writeLoginEnrollmentToken(path, token string) error {
	return atomicfile.Write(path, []byte(token), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
