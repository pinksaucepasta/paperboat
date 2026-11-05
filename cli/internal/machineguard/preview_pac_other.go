//go:build !darwin

package machineguard

import "context"

// Only macOS has a system-wide auto-proxy setting the guard owns.
func (g *guardServer) startPreviewPAC(context.Context) func() { return func() {} }
