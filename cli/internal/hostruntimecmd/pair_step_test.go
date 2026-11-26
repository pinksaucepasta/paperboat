package hostruntimecmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPairStepOffATerminalPrintsThePlainLineOnly(t *testing.T) {
	var out bytes.Buffer
	step := newPairStepFor(&out, "Connecting to Paperboat...", false)
	step.spin()
	step.end(false)
	if got := out.String(); got != "Connecting to Paperboat...\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestPairStepOnATerminalSpinsThenLeavesOneFinishedLine(t *testing.T) {
	for _, ok := range []bool{true, false} {
		var out bytes.Buffer
		step := newPairStepFor(&out, "Connecting to Paperboat...", true)
		if out.Len() != 0 {
			t.Fatalf("printed before spin: %q", out.String())
		}
		step.spin()
		time.Sleep(250 * time.Millisecond)
		step.end(ok)
		step.end(!ok) // later calls do nothing
		got := out.String()
		mark := map[bool]string{true: "✓", false: "✗"}[ok]
		if !strings.Contains(got, "\r\x1b[2K⠋ Connecting") {
			t.Fatalf("no spinner frame: %q", got)
		}
		if !strings.HasSuffix(got, "\r\x1b[2K"+mark+" Connecting to Paperboat...\n") || strings.Count(got, "\n") != 1 {
			t.Fatalf("line not cleaned up with %s: %q", mark, got)
		}
	}
}

func TestPairStepStopsSpinningAtTheFirstHelperByte(t *testing.T) {
	var out bytes.Buffer
	step := newPairStepFor(&out, "Starting the machine services...", true)
	step.spin()
	time.Sleep(100 * time.Millisecond)
	if _, err := step.stopOnWrite(&out).Write([]byte("helper says hi\n")); err != nil {
		t.Fatal(err)
	}
	step.spin()
	step.end(true)
	if got := out.String(); !strings.HasSuffix(got, "\r\x1b[2KStarting the machine services...\nhelper says hi\n") {
		t.Fatalf("helper output not after a plain line, or frames resumed: %q", got)
	}
}

func TestPairStepPendingEnvironmentEndsWithABangNotAFailure(t *testing.T) {
	var out bytes.Buffer
	step := newPairStepFor(&out, "Setting up encrypted environment...", true)
	step.spin()
	step.endPending(true)
	if got := out.String(); !strings.HasSuffix(got, "\r\x1b[2K! Setting up encrypted environment...\n") || strings.Contains(got, "✗") {
		t.Fatalf("pending step = %q", got)
	}
	out.Reset()
	newPairStepFor(&out, "Setting up encrypted environment...", true).endPending(false)
	if got := out.String(); got != "\r\x1b[2K✓ Setting up encrypted environment...\n" {
		t.Fatalf("ready step = %q", got)
	}
}
