//go:build darwin

package managedssh

import "os/exec"

// bindChildToParent does nothing on macOS: it has no kernel parent-death signal for a child that does not
// cooperate (the native tool is not ours to arm), so a pb killed with SIGKILL leaves its tool running there.
// SIGINT, SIGTERM and SIGHUP are forwarded by Execute.
func bindChildToParent(*exec.Cmd) (release func()) { return func() {} }
