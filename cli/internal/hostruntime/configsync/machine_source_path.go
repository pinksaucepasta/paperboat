package configsync

import (
	"github.com/pinksaucepasta/paperboat/cli/internal/userpaths"
	"runtime"
)

// DefaultMachineSourcePath is the single authoritative configuration file for this user and machine.
func DefaultMachineSourcePath() (string, error) {
	name := "Paperboat/config-sync.jsonc"
	if runtime.GOOS == "linux" {
		name = "paperboat/config-sync.jsonc"
	}
	return userpaths.Config(name)
}
