//go:build !linux

package nodemetrics

import (
	"errors"
	"runtime"
)

func cpuCount() int { return max(1, runtime.NumCPU()) }

// Nodes run on Linux; elsewhere (a developer's machine) there are no process statistics to report.
func readProcess() (float64, uint64, error) { return 0, 0, errors.New("process statistics need Linux") }
