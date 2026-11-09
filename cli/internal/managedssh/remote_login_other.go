//go:build !darwin

package managedssh

// SystemRemoteLoginDenied: only macOS has a Remote Login ACL; elsewhere sshd's own configuration decides.
func SystemRemoteLoginDenied(string) bool { return false }
