//go:build unix

package main

import "os"

func ownerOnlyRegularFile(_ string, info os.FileInfo) bool {
	return validateOwnerOnlyRegularFile("", info) == nil
}

func validateOwnerOnlyRegularFile(_ string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errOwnerOnlyFileInvalid
	}
	return nil
}
