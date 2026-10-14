package configsync

import (
	"github.com/pinksaucepasta/paperboat/internal/userpaths"
	"runtime"
)

// DefaultMachineSourcePath is the single authoritative configuration file for this user and machine.
func DefaultMachineSourcePath() (string, error) {
	name := "Paperboat/config-sync.toml"
	if runtime.GOOS == "linux" {
		name = "paperboat/config-sync.toml"
	}
	return userpaths.Config(name)
}
