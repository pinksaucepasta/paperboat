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
	settled  bool
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
	if !s.tty || s.stop != nil || s.settled {
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
	mark := "✓ "
	if !ok {
		mark = "✗ "
	}
	s.finish(mark)
}

// endPending ends with "!" when the step is left pending (its message follows), else with ✓.
func (s *pairStep) endPending(pending bool) {
	if pending {
		s.finish("! ")
		return
	}
	s.finish("✓ ")
}

// plain stops the frames and leaves the plain line, so a prompt or other output can follow; spin
// and end do nothing afterwards.
func (s *pairStep) plain() { s.finish("") }

func (s *pairStep) finish(mark string) {
	s.stopOnce.Do(func() {
		s.settled = true
		if !s.tty {
			return
		}
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
		fmt.Fprintf(s.w, "\r\x1b[2K%s%s\n", mark, s.text)
	})
}

// stopOnWrite returns w, which first leaves the plain line on its first byte.
func (s *pairStep) stopOnWrite(w io.Writer) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		s.plain()
		return w.Write(p)
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
