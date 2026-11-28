//go:build darwin || linux

package pty

import (
	"os"
	"os/signal"
	"syscall"
)

// A shell inherits an ignored SIGHUP from the process that starts it (exec keeps SIG_IGN; only handled signals
// return to their default). Under `nohup go test` every shell these tests start ignored the hangup, so
// Terminate waited out its whole grace (smoke 1128 suites on linuxpool-11 and macpool-2). With a handler
// installed here the shells get the default SIGHUP however the test binary was launched.
func init() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
}
