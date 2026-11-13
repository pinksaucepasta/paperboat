//go:build darwin || linux

package runtime

// Unix state is owned by the user's uid; only Windows ACLs can name a session that no longer exists.
func repairConfigSyncStateAccess(string) error { return nil }
