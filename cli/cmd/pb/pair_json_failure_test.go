package main

import (
	"strings"
	"testing"
)

func TestPairJSONFailureCarriesRuntimeCause(t *testing.T) {
	var output boundedTailBuffer
	_, _ = output.Write([]byte(strings.Repeat("x", 2*boundedTailBufferSize) + "\npb: This enrollment was already used.\n"))
	message := runtimeFailureLine(output.String(), "Machine pairing failed.")
	if message != "This enrollment was already used." {
		t.Fatalf("message=%q", message)
	}
	if got := classifyCLIJSONError(jsonFailureExitCodeError{code: 1, message: message}).Message; got != message {
		t.Fatalf("json message=%q", got)
	}
	if got := runtimeFailureLine("", "Machine pairing failed."); got != "Machine pairing failed." {
		t.Fatalf("fallback=%q", got)
	}
}
