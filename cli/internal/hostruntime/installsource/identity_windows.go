//go:build windows

package installsource

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileIdentity is a regular file's exact NTFS identity: volume, file ID, size, last write and change time. Any write,
// replacement, rename onto the path or security change through the file system changes it. Without it the updater's
// 5 s reconciliation re-hashed up to four ~100 MB executables every pass on Windows: victus read ~52 MB/s nonstop and
// a concurrent install ran out of its activation budget (2026-11-13).
type fileIdentity struct {
	volume                uint32
	indexHigh, indexLow   uint32
	size                  int64
	lastWrite, changeTime int64
}

// fileBasicInfo is FILE_BASIC_INFO (x/sys/windows has the class constant but not the struct).
type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

func fileIdentityOf(path string) (fileIdentity, bool) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileIdentity{}, false
	}
	// Like Lstat: a reparse point is opened itself, never followed.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fileIdentity{}, false
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &info) != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fileIdentity{}, false
	}
	var basic fileBasicInfo
	if windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))) != nil {
		return fileIdentity{}, false
	}
	return fileIdentity{
		volume:    info.VolumeSerialNumber,
		indexHigh: info.FileIndexHigh, indexLow: info.FileIndexLow,
		size:      int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow),
		lastWrite: basic.LastWriteTime, changeTime: basic.ChangeTime,
	}, true
}
