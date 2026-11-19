//go:build windows

package main

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

// Input handed over as the daemon link drops reports ERROR_NO_DATA (232), not EPIPE: it must close input quietly so
// the exec recovers instead of ending before a replacement attachment is opened.
func TestExecInputClosedOnlyAcceptsTheWindowsClosedPipeErrnos(t *testing.T) {
	for _, errno := range []error{windows.ERROR_BROKEN_PIPE, windows.ERROR_NO_DATA, windows.ERROR_PIPE_NOT_CONNECTED} {
		if !execInputClosedOnly(fmt.Errorf("close input: %w", errno)) {
			t.Fatalf("%v ended the exec instead of closing its input", errno)
		}
	}
}
