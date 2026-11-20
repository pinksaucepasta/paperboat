package main

import (
	"os"
	"testing"
)

// /dev/null is a character device but not a terminal: a prompt on it would read end of file (the fresh install then
// refused with "no valid region given" under cron and `go test`).
func TestNullDeviceIsNotATerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Fatal("/dev/null was taken for a terminal")
	}
}
