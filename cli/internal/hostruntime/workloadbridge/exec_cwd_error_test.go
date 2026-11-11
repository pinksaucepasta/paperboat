package workloadbridge

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
)

// The workspace refusal crosses the owner boundary as itself and still matches ErrInvalid.
func TestWorkingDirectoryRefusalSurvivesTheBridge(t *testing.T) {
	decoded := decodeError(encodeError(execprocess.ErrWorkingDirectory))
	if !errors.Is(decoded, execprocess.ErrWorkingDirectory) || !errors.Is(decoded, execprocess.ErrInvalid) {
		t.Fatalf("decoded = %v", decoded)
	}
}
