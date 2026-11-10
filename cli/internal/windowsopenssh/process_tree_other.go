//go:build !windows

package windowsopenssh

import "os/exec"

func containProcessTree(*exec.Cmd) (func(), error) { return func() {}, nil }
