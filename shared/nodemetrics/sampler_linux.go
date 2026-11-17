//go:build linux

package nodemetrics

import (
	"os"
	"runtime"
)

// clockTicks is USER_HZ, which is 100 on every Linux the nodes run on (it is fixed by the kernel ABI).
const clockTicks = 100

func cpuCount() int { return max(1, runtime.NumCPU()) }

func readProcess() (float64, uint64, error) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, 0, err
	}
	ticks, err := parseStat(string(stat))
	if err != nil {
		return 0, 0, err
	}
	statm, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, 0, err
	}
	pages, err := parseStatm(string(statm))
	if err != nil {
		return 0, 0, err
	}
	return float64(ticks) / clockTicks, pages * uint64(os.Getpagesize()), nil
}
