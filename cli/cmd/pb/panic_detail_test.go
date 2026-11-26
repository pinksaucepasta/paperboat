package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func panickingInventoryStep() { panic("inventory step broke") }

// A panic's site must survive: root host-service steps keep no diagnostics file, so the crash of a revoked machine's
// install inventory printed only "Paperboat stopped unexpectedly" and its site was lost.
func TestPanicDetailNamesThePanicSite(t *testing.T) {
	var output bytes.Buffer
	func() {
		defer func() { writePanicDetail(&output, recover(), debug.Stack()) }()
		panickingInventoryStep()
	}()
	for _, want := range []string{"pb: panic: inventory step broke", "panickingInventoryStep"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("panic detail lacks %q: %q", want, output.String())
		}
	}
	output.Reset()
	writePanicDetail(&output, "x", bytes.Repeat([]byte("f"), 3*maxPanicStack))
	if output.Len() > maxPanicStack+128 || !strings.Contains(output.String(), "(truncated)") {
		t.Fatalf("stack is not bounded: %d bytes", output.Len())
	}
}
