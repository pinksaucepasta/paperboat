package bugreport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type unsupportedRecordingReader struct{}

func (unsupportedRecordingReader) Read([]byte) (int, error) {
	panic("unsupported input must never be read")
}

func TestRecordingInputKnownFiniteAndUnsupported(t *testing.T) {
	for _, reader := range []io.Reader{bytes.NewBufferString("PRIVATE_LINE\nremaining"), bytes.NewReader([]byte("PRIVATE_LINE\nremaining")), strings.NewReader("PRIVATE_LINE\nremaining")} {
		if err := waitForLine(t.Context(), reader); err != nil {
			t.Fatal(err)
		}
		remaining, _ := io.ReadAll(reader)
		if string(remaining) != "remaining" {
			t.Fatal("confirmation consumed following input")
		}
	}
	if err := waitForLine(t.Context(), strings.NewReader("unterminated")); err != nil {
		t.Fatal(err)
	}
	if err := waitForLine(t.Context(), unsupportedRecordingReader{}); !errors.Is(err, errRecordingInput) {
		t.Fatal("unsupported reader accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	finite := strings.NewReader("\n")
	if err := waitForLine(ctx, finite); !errors.Is(err, context.Canceled) || finite.Len() != 1 {
		t.Fatal("canceled finite input consumed")
	}
}

func TestRecordingInputBorrowedRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE_INPUT")
	if err := os.WriteFile(path, []byte("PRIVATE_LINE\nremaining"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := waitForLine(t.Context(), file); err != nil {
		t.Fatal(err)
	}
	remaining, _ := io.ReadAll(file)
	if string(remaining) != "remaining" {
		t.Fatal("borrowed file closed or input overconsumed")
	}
	file.Close()
	err = waitForLine(t.Context(), file)
	var original *os.PathError
	if !errors.As(err, &original) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("typed safe input failure lost")
	}
}

func TestRecordingInputBorrowedPipeCancellationAndReuse(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- waitForLine(ctx, reader) }()
	time.Sleep(25 * time.Millisecond)
	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("blocked pipe cancellation cause lost")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("blocked pipe confirmation did not finish")
	}
	if time.Since(began) > 250*time.Millisecond {
		t.Fatal("pipe cancellation exceeded bound")
	}
	if _, err := writer.Write([]byte("PRIVATE_LINE\nremaining")); err != nil {
		t.Fatal(err)
	}
	if err := waitForLine(t.Context(), reader); err != nil {
		t.Fatal(err)
	}
	var rest [9]byte
	if _, err := io.ReadFull(reader, rest[:]); err != nil || string(rest[:]) != "remaining" {
		t.Fatal("borrowed pipe unusable after cancellation")
	}
	writer.Close()
	if err := waitForLine(t.Context(), reader); err != nil {
		t.Fatal("pipe EOF no longer completes confirmation")
	}
}
