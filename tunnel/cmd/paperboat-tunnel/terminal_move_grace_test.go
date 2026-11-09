package main

import (
	"testing"
	"time"
)

// The terminals still open are moved before the drain ends, whatever the shutdown timeout is.
func TestTerminalMoveGraceIsDerivedFromTheDrainWindow(t *testing.T) {
	for window, want := range map[time.Duration]time.Duration{
		2 * time.Second:   time.Second,
		15 * time.Second:  7500 * time.Millisecond, // the default 30 s shutdown timeout
		150 * time.Second: 10 * time.Second,        // a 5 m shutdown timeout
	} {
		if got := terminalMoveGrace(window); got != want || got >= window {
			t.Errorf("window %v: grace %v, want %v and below the window", window, got, want)
		}
	}
}
