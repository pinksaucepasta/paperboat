package hostruntimecmd

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"golang.org/x/term"
)

// pairStep is one pair step line. Off a terminal it prints the plain line at once and nothing else.
// On a terminal it prints nothing until spin, which must come only after any approval prompt has
// returned; end replaces the spinning frame with ✓ or ✗. Nothing else may write to the terminal
// between spin and end.
type pairStep struct {
	w        io.Writer
	text     string
	tty      bool
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newPairStep(w io.Writer, text string) *pairStep {
	file, ok := w.(*os.File)
	tty := ok && term.IsTerminal(int(file.Fd())) && os.Getenv("TERM") != "dumb"
	return newPairStepFor(w, text, tty)
}

func newPairStepFor(w io.Writer, text string, tty bool) *pairStep {
	if !tty {
		fmt.Fprintln(w, text)
	}
	return &pairStep{w: w, text: text, tty: tty}
}

// spin starts the frames; a no-op off a terminal or once started.
func (s *pairStep) spin() {
	if !s.tty || s.stop != nil {
		return
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	frames := spinner.MiniDot
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(frames.FPS)
		defer ticker.Stop()
		for i := 0; ; i++ {
			fmt.Fprintf(s.w, "\r\x1b[2K%s %s", frames.Frames[i%len(frames.Frames)], s.text)
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// end stops the frames and leaves the finished line; later calls do nothing.
func (s *pairStep) end(ok bool) {
	s.stopOnce.Do(func() {
		if !s.tty {
			return
		}
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		fmt.Fprintf(s.w, "\r\x1b[2K%s %s\n", mark, s.text)
	})
}
