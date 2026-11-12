package diskspace

import (
	"os"
	"syscall"
	"testing"
)

func TestIsFullRecognisesTheWindowsDiskFullCodes(t *testing.T) {
	for _, code := range []syscall.Errno{39, 112} {
		if !IsFull(&os.PathError{Op: "write", Path: "x", Err: code}) {
			t.Fatalf("error %d not recognised as a full disk", code)
		}
	}
}
