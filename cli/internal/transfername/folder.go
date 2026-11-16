package transfername

import (
	"fmt"
	"strings"
)

// Bounds of a folder send, one place for the sender (which refuses before anything is sent) and the receiver (which
// refuses again when it unpacks).
const (
	MaxFolderEntries   = 100_000 // files and folders inside one sent folder
	MaxFolderPathBytes = 1024    // longest path inside a sent folder, slash separated
)

// FolderPathRefusal returns why a receiver on platform cannot store rel, a slash-separated path inside a sent folder
// (never absolute, no "." or ".." part), or "" when it can. Every part must be a name the receiver stores as a single
// file (Refusal), so a folder is held to the same rules as its files sent one by one.
func FolderPathRefusal(platform, rel string) string {
	if rel == "" || len(rel) > MaxFolderPathBytes {
		return fmt.Sprintf("the path is empty or longer than %d bytes", MaxFolderPathBytes)
	}
	for _, part := range strings.Split(rel, "/") {
		if reason := Refusal(platform, part); reason != "" {
			return reason
		}
	}
	return ""
}

// FolderCollision names two paths of a folder that a receiver on platform would store as one: Windows and macOS compare
// names without regard to case, so "a.txt" and "A.txt" overwrite each other there. It returns "" when no two collide.
func FolderCollision(platform string, paths []string) string {
	if platform != "windows" && platform != "darwin" {
		return ""
	}
	seen := make(map[string]string, len(paths))
	for _, path := range paths {
		key := strings.ToLower(path)
		if other, ok := seen[key]; ok {
			return fmt.Sprintf("%q and %q are the same name there (names are not case-sensitive)", other, path)
		}
		seen[key] = path
	}
	return ""
}
