//go:build !windows

package endpointbinary

import "path/filepath"

func resolveExecutablePath(path string) (string, error) { return filepath.EvalSymlinks(path) }
