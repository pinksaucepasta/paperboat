//go:build !windows

package configsync

import "os"

func CreateSourceFile(path string) (*os.File, error) {
	if !canonicalAbsolutePath(path) {
		return nil, ErrSourceConfigInvalid
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}
