//go:build windows

package endpointbinary

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// Resolve the opened executable rather than enumerating each ancestor. The
// enrolled user can execute the protected installation without permission to
// list its administrative parent directory. Opening normally still follows
// reparse points, so callers receive the actual target rather than a lexical path.
func resolveExecutablePath(path string) (resolved string, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &os.PathError{Op: "resolve executable", Path: path, Err: resultErr}
		}
	}()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	buffer := make([]uint16, 512)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length == 0 || length > 32<<10 {
			return "", os.ErrInvalid
		}
		if int(length) >= len(buffer) {
			buffer = make([]uint16, int(length)+1)
			continue
		}
		resolved = windows.UTF16ToString(buffer[:length])
		if strings.HasPrefix(resolved, `\\?\UNC\`) {
			return `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`), nil
		}
		return strings.TrimPrefix(resolved, `\\?\`), nil
	}
}
