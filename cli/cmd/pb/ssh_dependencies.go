package main

import "context"

// sshDependencies are the edges pb ssh reaches that tests replace.
type sshDependencies struct {
	// refreshInventory asks the running daemon to re-read the account's machines: pb ssh reads host keys from the
	// daemon's machine list, so a connection and a host trust both refresh it.
	refreshInventory func(context.Context) error
}

func defaultSSHDependencies() sshDependencies {
	return sshDependencies{refreshInventory: refreshLocalInventory}
}

func sshDependenciesOf(ctx context.Context) sshDependencies {
	return dependenciesIn(ctx, defaultSSHDependencies)
}
