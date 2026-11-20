//go:build unix

package main

import "testing"

func TestWarnIfFileLimitLow(t *testing.T) {
	have, ok := openFileLimit()
	if !ok {
		t.Skip("no file limit on this platform")
	}
	if have > 1<<30 {
		t.Skip("file limit is effectively unlimited")
	}
	if !warnIfFileLimitLow(1) {
		t.Fatalf("a limit of %d is below 1+%d", have, fileLimitMargin)
	}
	if warnIfFileLimitLow(int(have)) {
		t.Fatalf("a limit of %d cannot hold %d listeners and the margin", have, have)
	}
}
