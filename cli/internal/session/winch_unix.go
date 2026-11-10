//go:build !windows

package session

import (
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

func notifyWinch(ch chan<- os.Signal) (stop func()) {
	signal.Notify(ch, unix.SIGWINCH)
	return func() { signal.Stop(ch) }
}
