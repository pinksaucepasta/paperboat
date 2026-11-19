//go:build !windows

package main

// execPipeClosed has no additional closed-pipe errnos off Windows: the portable set in execInputClosedOnly already
// covers a dropped link there.
func execPipeClosed(error) bool { return false }
