//go:build unix

package configsync

import (
	"os"
	"syscall"
)

func privateRepositoryCredentialDirectory(_ string, info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func protectRepositoryCredentialDirectory(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrRepositoryCredentials
	}
	return os.Chmod(path, 0700)
}

func secureRepositoryReference(path string, info os.FileInfo, private bool) bool {
	return info.Mode().Perm()&0022 == 0 && (!private || privateControlFile(path, info))
}

func repositoryReferencePathMatches(path, resolved string) bool { return path == resolved }

func createRepositoryCredentialDirectory(path string) error { return os.Mkdir(path, 0700) }
