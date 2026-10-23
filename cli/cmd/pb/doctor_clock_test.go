package main

import (
	"testing"
	"time"
)

func TestDoctorNamesTheClockOffset(t *testing.T) {
	for offset, want := range map[time.Duration]string{
		10 * time.Minute: "10 minutes behind",
		-time.Minute:     "1 minute ahead",
		-3 * time.Hour:   "3 hours ahead",
		90 * time.Second: "2 minutes behind",
	} {
		if got := describeClockOffset(offset); got != want {
			t.Errorf("%v: %q, want %q", offset, got, want)
		}
	}
}
