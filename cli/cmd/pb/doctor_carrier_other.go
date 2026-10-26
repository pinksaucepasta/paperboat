//go:build !linux

package main

import doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"

// Only systemd reports a unit that keeps failing to start in a way doctor can read without privileges.
func doctorCarrierProbes() []doctorpkg.Probe { return nil }
