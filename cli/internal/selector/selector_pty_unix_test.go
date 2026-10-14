//go:build !windows

package selector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestChooseRestoresTerminalAndSelects(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	type result struct {
		item Item
		err  error
	}
	done := make(chan result, 1)
	go func() {
		item, chooseErr := Choose(Options{Title: "Choose", Items: []Item{{ID: "one", Title: "One"}}, Stdin: slave, Output: &output})
		done <- result{item: item, err: chooseErr}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := master.Write([]byte{'\r'}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.item.ID != "one" {
			t.Fatalf("choose = %+v, %v", got.item, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selector did not handle input")
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("selector did not restore terminal state")
	}
	if rendered := output.String(); !strings.Contains(rendered, "\x1b[?1049h") || !strings.Contains(rendered, "\x1b[?2004l") || !strings.Contains(rendered, "\x1b[?1049l") || !strings.HasSuffix(rendered, "\x1b[?25h") {
		t.Fatalf("terminal lifecycle output = %q", rendered)
	}
}

func TestChooseRequiredFilterUpdatesFromTerminalInput(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	type result struct {
		item Item
		err  error
	}
	done := make(chan result, 1)
	go func() {
		item, chooseErr := Choose(Options{
			Title:         "Send files",
			Items:         []Item{{ID: "report", Title: "report.pdf"}, {ID: "notes", Title: "notes.txt"}},
			RequireFilter: true,
			Stdin:         slave,
			Output:        &bytes.Buffer{},
		})
		done <- result{item: item, err: chooseErr}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := master.Write([]byte("notes\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.item.ID != "notes" {
			t.Fatalf("choose = %+v, %v", got.item, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("filtered selector did not handle terminal input")
	}
}

func TestChooseRecognizesBracketedPasteFromTerminal(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	type result struct {
		item Item
		err  error
	}
	done := make(chan result, 1)
	go func() {
		item, chooseErr := Choose(Options{
			Title: "Serve a file or directory", Items: []Item{{ID: "ordinary", Title: "ordinary"}}, Stdin: slave, Output: &bytes.Buffer{},
			InputSelection: func(value string) (Item, bool) {
				if value == "/tmp/My Report.pdf" {
					return Item{ID: "dropped", Title: "My Report.pdf"}, true
				}
				return Item{}, false
			},
		})
		done <- result{item: item, err: chooseErr}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := master.Write([]byte("\x1b[200~/tmp/My Report.pdf\x1b[201~")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.item.ID != "dropped" {
			t.Fatalf("bracketed paste selection = %+v, %v", got.item, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selector did not recognize bracketed paste")
	}
}

func TestChooserInsideSuspendedScreenOwnsItsTerminal(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	var output bytes.Buffer
	end := BeginScreen(&output)
	defer end()
	restore := SuspendScreen(&output)
	defer restore()
	output.Reset()
	done := make(chan error, 1)
	go func() {
		_, err := Choose(Options{Title: "Nested prompt", Items: []Item{{ID: "one", Title: "One"}}, Stdin: slave, Output: &output})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	_, _ = master.Write([]byte{'\r'})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nested selector did not finish")
	}
	if !strings.Contains(output.String(), "\x1b[?1049h") || !strings.Contains(output.String(), "\x1b[?1049l") {
		t.Fatal("nested selector failed to own and restore its alternate screen")
	}
}

// The public chooser retains restoration/output failure without rendering it as
// error text, and can open a fresh selector afterward.
func TestChoosePreservesOutputFailureAndRecovery(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	for _, output := range []io.Writer{selectorFailingWriter{}, io.Discard} {
		done := make(chan error, 1)
		go func() {
			_, err := Choose(Options{Context: context.Background(), Title: "PRIVATE", Items: []Item{{ID: "one"}}, Stdin: slave, Output: output})
			done <- err
		}()
		time.Sleep(100 * time.Millisecond)
		if _, err := master.Write([]byte{'\r'}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if _, failing := output.(selectorFailingWriter); failing {
				if !errors.Is(err, syscall.EIO) || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("unsafe or lost output failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("selector did not finish")
		}
	}
}

type selectorFailingWriter struct{}

func (selectorFailingWriter) Write([]byte) (int, error) { return 0, syscall.EIO }

func TestChoosePreservesContextCause(t *testing.T) {
	for _, preCanceled := range []bool{false, true} {
		t.Run(fmt.Sprint(preCanceled), func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			if preCanceled {
				cancel(syscall.EIO)
			}
			done := make(chan error, 1)
			go func() {
				_, err := Choose(Options{Context: ctx, Items: []Item{{ID: "one"}}, Stdin: slave, Output: io.Discard})
				done <- err
			}()
			if !preCanceled {
				time.Sleep(100 * time.Millisecond)
				cancel(syscall.EIO)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) {
					t.Fatalf("context cause lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("canceled selector did not finish")
			}
		})
	}
}
