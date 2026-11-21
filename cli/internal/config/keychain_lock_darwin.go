//go:build darwin

package config

import (
	"sync"

	"github.com/ebitengine/purego"
)

// The keychain lock state is read in process through Security.framework, reached with purego rather than cgo
// so release builds stay CGO_ENABLED=0 (the NetBird pattern, ~/ref/netbird/client/internal/certproof/keychain_darwin.go).
// SecKeychainGetStatus never shows UI, unlike the `security` child go-keyring runs, which waits on the unlock dialog.
var (
	keychainStatusOnce      sync.Once
	secKeychainCopyDefault  func(keychain *uintptr) int32
	secKeychainGetStatus    func(keychain uintptr, status *uint32) int32
	keychainStatusCFRelease func(ref uintptr)
	keychainStatusLoaded    bool
)

const (
	kSecUnlockStateStatus   = 1
	errSecNoDefaultKeychain = -25307
)

func init() { keychainLocked = darwinKeychainLocked }

func darwinKeychainLocked() bool {
	keychainStatusOnce.Do(func() {
		security, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		foundation, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		purego.RegisterLibFunc(&secKeychainCopyDefault, security, "SecKeychainCopyDefault")
		purego.RegisterLibFunc(&secKeychainGetStatus, security, "SecKeychainGetStatus")
		purego.RegisterLibFunc(&keychainStatusCFRelease, foundation, "CFRelease")
		keychainStatusLoaded = true
	})
	if !keychainStatusLoaded {
		return false // unknown: the bounded `security` call still answers
	}
	var keychain uintptr
	switch status := secKeychainCopyDefault(&keychain); {
	case status == errSecNoDefaultKeychain:
		return true // a second OS user with no login keychain: `security` would wait just the same
	case status != 0 || keychain == 0:
		return false
	}
	defer keychainStatusCFRelease(keychain)
	var status uint32
	if secKeychainGetStatus(keychain, &status) != 0 {
		return false
	}
	return status&kSecUnlockStateStatus == 0
}
