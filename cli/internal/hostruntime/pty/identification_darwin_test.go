//go:build darwin

package pty

import (
	"testing"
	"unsafe"
)

func TestProcVnodePathInfoABI(t *testing.T) {
	var paths procVnodePathInfo
	if unsafe.Sizeof(paths) != 2352 || unsafe.Offsetof(paths.Current.Path) != 152 {
		t.Fatal("proc_vnodepathinfo does not match Apple's 64-bit ABI")
	}
}
