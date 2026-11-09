//go:build windows

package windowssecurity

import "golang.org/x/sys/windows"

// LongPath spells an existing path with its long names. GetFinalPathNameByHandle answers with long names, while
// TEMP, USERPROFILE and paths typed by users can hold 8.3 short names (C:\Users\RUNNER~1): the same object, a
// different string. Compare LongPath of both sides. A path that does not exist is returned unchanged.
func LongPath(path string) string {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	size := uint32(windows.MAX_PATH)
	for attempt := 0; attempt < 2; attempt++ {
		buffer := make([]uint16, size)
		n, err := windows.GetLongPathName(encoded, &buffer[0], size)
		if err != nil || n == 0 {
			return path
		}
		if n < size {
			return windows.UTF16ToString(buffer[:n])
		}
		size = n + 1
	}
	return path
}
