//go:build windows

package main

import "testing"

// No daemon listening is a typed dial failure and means nothing is open; the update proceeds.
func TestOpenConnectionsOverAMissingPipeCountsNone(t *testing.T) {
	open, err := openConnectionsOver(t.Context(), `\\.\pipe\paperboat-no-such-daemon-test`)
	if err != nil || open.Total() != 0 {
		t.Fatalf("open=%+v err=%v", open, err)
	}
}
