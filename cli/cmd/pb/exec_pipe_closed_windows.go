//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// execPipeClosed reports the Windows named-pipe errnos that say the daemon link is gone: the peer closed while input
// was being handed over. They join the portable closed-pipe set in execInputClosedOnly; a resend keeps the bytes in
// the journal, so treating them as EOF loses nothing.
func execPipeClosed(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}
