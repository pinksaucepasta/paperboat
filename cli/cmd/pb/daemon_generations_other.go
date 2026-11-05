//go:build !darwin && !linux

package main

import (
	"io"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// Daemon generations drain side by side only on Linux and macOS.
func writeDaemonGenerations(io.Writer)                {}
func doctorDaemonGenerationProbes() []doctorpkg.Probe { return nil }
