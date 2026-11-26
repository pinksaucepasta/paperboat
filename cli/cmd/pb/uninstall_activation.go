package main

import (
	"context"
	"time"
)

// uninstallActivationWait bounds how long uninstall waits for a running update activation to finish.
var uninstallActivationWait, uninstallActivationPoll = 2 * time.Minute, 2 * time.Second

// waitForUpdateActivation waits, bounded, for an update activation to finish before uninstall removes anything:
// removing services under a running activator leaves both versions half-installed. Still running: refuse.
func waitForUpdateActivation(ctx context.Context, activating func() (bool, error)) error {
	deadline := time.Now().Add(uninstallActivationWait)
	for {
		running, err := activating()
		if err != nil || !running {
			return nil // a state that cannot be read proceeds, like refuseUpdateWhileActivating
		}
		if time.Now().After(deadline) {
			return commandRejection{reason: commandRejectUpdateActivating, detail: "An update is being activated. Run `pb update status` to follow it, then run `pb uninstall` again."}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(uninstallActivationPoll):
		}
	}
}
