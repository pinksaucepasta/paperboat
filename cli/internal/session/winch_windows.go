//go:build windows

package session

import (
	"os"
	"time"

	"golang.org/x/term"
)

// Windows has no SIGWINCH, and stdin is read as a byte stream, so console
// resize events are not available either: poll the console size, as the
// Docker CLI does (cli/command/container/tty.go MonitorTtySize).
func notifyWinch(ch chan<- os.Signal) (stop func()) {
	ticker := time.NewTicker(winchPollInterval)
	done := make(chan struct{})
	go pollWinch(ch, func() (int, int, error) { return term.GetSize(int(os.Stdout.Fd())) }, ticker.C, done)
	return func() {
		ticker.Stop()
		close(done)
	}
}
