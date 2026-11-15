package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

// startCPUProfile is the idle-cost diagnostic (PR-03): with PAPERBOAT_CPU_PROFILE=<directory> set in a service's
// environment, each pb process writes <directory>/<pid>-<role>.pprof for PAPERBOAT_CPU_PROFILE_SECONDS (default 300),
// then stops. `go tool pprof -top <file>` reads it.
func startCPUProfile() {
	directory := os.Getenv("PAPERBOAT_CPU_PROFILE")
	if directory == "" {
		return
	}
	seconds, err := strconv.Atoi(os.Getenv("PAPERBOAT_CPU_PROFILE_SECONDS"))
	if err != nil || seconds < 1 {
		seconds = 300
	}
	role := "pb"
	if len(os.Args) > 2 {
		role = strings.Trim(os.Args[2], "_-")
	}
	file, err := os.Create(filepath.Join(directory, fmt.Sprintf("%d-%s.pprof", os.Getpid(), role)))
	if err != nil {
		return
	}
	if pprof.StartCPUProfile(file) != nil {
		file.Close()
		return
	}
	time.AfterFunc(time.Duration(seconds)*time.Second, func() { pprof.StopCPUProfile(); file.Close() })
}
