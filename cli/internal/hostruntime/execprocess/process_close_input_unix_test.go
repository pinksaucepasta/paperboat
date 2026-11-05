//go:build darwin || linux

package execprocess

import (
	"context"
	"testing"
)

// pbvm4 prefix1 07:48:07.888: `pb exec -- true` exited before the client's stdin EOF arrived; os/exec's
// Wait had already closed the stdin pipe, the second Close failed and the owner ended the exec stream,
// so the CLI reattached and the call took 1.3 s more.
func TestPipeProcessCloseInputAfterExitSucceeds(t *testing.T) {
	process, err := newProcess(processConfig{Request: Request{Argv: []string{"true"}, CWD: t.TempDir()}, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, ChunkBytes: 1024, Output: func(string, []byte) {}})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := process.CloseInput(); err != nil {
		t.Fatalf("CloseInput after exit = %v, want nil", err)
	}
}
