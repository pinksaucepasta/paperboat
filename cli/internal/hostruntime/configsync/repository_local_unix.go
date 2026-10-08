//go:build !windows

package configsync

import "os"

func syncRepositoryDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func renameRepositoryReference(source, target string) error { return os.Rename(source, target) }
