//go:build !darwin && !linux

package main

import doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"

// The Windows host service is checked by its service manager, not by a socket.
func doctorHostdProbes() []doctorpkg.Probe { return nil }
