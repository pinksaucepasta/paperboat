package machineservices

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

// readBounded returns exactly what a reader holds up to the limit, reads in large steps, and refuses one byte over.
func TestReadBoundedKeepsItsLimitAndErrors(t *testing.T) {
	for _, size := range []int{0, 1, readBufferBytes - 1, readBufferBytes, readBufferBytes + 1, 3*readBufferBytes + 7} {
		want := bytes.Repeat([]byte("x"), size)
		for name, reader := range map[string]io.Reader{"plain": bytes.NewReader(want), "one byte at a time": iotest.OneByteReader(bytes.NewReader(want))} {
			if size > 1000 && name == "one byte at a time" {
				continue
			}
			got, err := readBounded(reader, int64(size))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%d bytes %s: got %d bytes, %v", size, name, len(got), err)
			}
		}
		if size > 0 {
			if _, err := readBounded(bytes.NewReader(want), int64(size-1)); !errors.Is(err, ErrSnapshotTooLarge) {
				t.Fatalf("%d bytes over a limit of %d: %v", size, size-1, err)
			}
		}
	}
	if _, err := readBounded(strings.NewReader("x"), -1); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("negative limit: %v", err)
	}
	failure := errors.New("read failed")
	if _, err := readBounded(io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(failure)), 100); !errors.Is(err, failure) {
		t.Fatalf("a read error was hidden: %v", err)
	}
}

// BenchmarkSocketTableRead: the same read the worker repeats every two seconds, on this machine's /proc/net/tcp.
func BenchmarkSocketTableRead(b *testing.B) {
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		b.Skip("no /proc/net/tcp")
	}
	b.Run("ReadAll", func(b *testing.B) {
		for range b.N {
			file, _ := os.Open("/proc/net/tcp")
			_, _ = io.ReadAll(io.LimitReader(file, maximumSnapshotBytes+1))
			file.Close()
		}
	})
	b.Run("readBounded", func(b *testing.B) {
		for range b.N {
			file, _ := os.Open("/proc/net/tcp")
			_, _ = readBounded(file, maximumSnapshotBytes)
			file.Close()
		}
	})
}
