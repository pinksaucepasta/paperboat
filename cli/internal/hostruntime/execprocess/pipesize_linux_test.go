//go:build linux

package execprocess

import (
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
)

// Exec output is read in chunks of the largest terminal frame, which needs a pipe that can hold one.
func TestEnlargedPipeHoldsAWholeChunk(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	before := pipeSize(reader)
	enlargePipe(reader, 128<<10*4)
	after := pipeSize(reader)
	if after < 128<<10 {
		t.Skipf("the kernel refused a bigger pipe (before %d, after %d): /proc/sys/fs/pipe-max-size", before, after)
	}
}

func TestDefaultExecChunkStaysWellBelowTheTerminalFrameLimit(t *testing.T) {
	manager := testManager(t, t.TempDir(), func(config *Config) { config.ChunkBytes = 0 })
	if got := manager.config.ChunkBytes; got != 128<<10 || got*2 > protocol.MaxTerminalOutputBytes+1<<10 {
		t.Fatalf("default chunk = %d", got)
	}
}
