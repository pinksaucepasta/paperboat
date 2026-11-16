//go:build !darwin

package main

import doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"

// Linux and Windows route all of 127/8, so the managed SSH source addresses always exist there.
func doctorSSHSourceProbes() []doctorpkg.Probe { return nil }
