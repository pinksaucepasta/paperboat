//go:build windows

package pty

// Identification is unavailable natively for ConPTY: its console handle does
// not expose the foreground process or that process's current directory. The
// runtime consumes application title and OSC 7 reports instead. Returning the
// initial process image or launch directory would misidentify running jobs.
func (p *Process) Identification() Identification { return Identification{} }
