package hostruntimecmd

import (
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localtestroot"
)

// TestMain keeps every test (and re-executed helper) off an installed Paperboat's state and pipes.
func TestMain(m *testing.M) {
	cleanup, err := localtestroot.Isolate()
	if err != nil {
		panic(err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
