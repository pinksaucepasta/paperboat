//go:build darwin

package pty

import (
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

var loadProcPIDInfo = sync.OnceValue(func() func(int32, int32, uint64, unsafe.Pointer, int32) int32 {
	handle, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil
	}
	symbol, err := purego.Dlsym(handle, "proc_pidinfo")
	if err != nil {
		_ = purego.Dlclose(handle)
		return nil
	}
	var call func(int32, int32, uint64, unsafe.Pointer, int32) int32
	purego.RegisterFunc(&call, symbol)
	// Retain this one library handle for the lifetime of the registered call.
	return call
})

// Layout from Apple's public bsd/sys/proc_info.h: vnode_info is 152 bytes,
// followed by a MAXPATHLEN (1024) path; proc_vnodepathinfo contains cwd/root.
// Both supported Darwin architectures use this fixed-width 64-bit ABI.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info.h
type vnodeInfoPath struct {
	Info [19]uint64
	Path [1024]byte
}

type procVnodePathInfo struct {
	Current vnodeInfoPath
	Root    vnodeInfoPath
}

func identifyForeground(group int) Identification {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", group)
	if err != nil {
		return Identification{}
	}
	// Query one process, never the system process inventory.
	result := Identification{ForegroundProcess: unix.ByteSliceToString(process.Proc.P_comm[:])}
	if call := loadProcPIDInfo(); call != nil {
		var paths procVnodePathInfo
		const procPIDVnodePathInfo = 9
		size := int32(unsafe.Sizeof(paths))
		if call(int32(group), procPIDVnodePathInfo, 0, unsafe.Pointer(&paths), size) == size {
			result.CurrentDirectory = unix.ByteSliceToString(paths.Current.Path[:])
		}
		runtime.KeepAlive(&paths)
	}
	return result
}
