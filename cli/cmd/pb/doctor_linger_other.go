//go:build !linux

package main

import doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"

// Only systemd hosts end a user's services at logout.
func doctorLingerProbes() []doctorpkg.Probe { return nil }
