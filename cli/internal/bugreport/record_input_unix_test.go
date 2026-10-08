//go:build darwin || linux

package bugreport

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"testing"
	"time"
)

func recordingFileFlags(t *testing.T, file *os.File) int {
	t.Helper()
	raw, err := file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	if err := raw.Control(func(fd uintptr) { flags, err = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return flags
}
func TestRecordingInputDoesNotChangeBorrowedPipeFlags(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	before := recordingFileFlags(t, reader)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := waitForLine(ctx, reader); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline lost")
	}
	if after := recordingFileFlags(t, reader); after != before {
		t.Fatal("borrowed pipe flags changed")
	}
}
