//go:build windows

package runtime

import "os"

// cacheFileTrusted: on Windows the cache lives in the account's own state directory, whose inherited DACL names only the
// account, SYSTEM and Administrators; Go reports no owner or mode for it, and a link is already refused by the caller.
// shortcut: no per-file owner check on Windows, add a DACL assertion if the state directory ever becomes shared.
func cacheFileTrusted(os.FileInfo) bool { return true }
