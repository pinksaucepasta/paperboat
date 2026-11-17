//go:build !darwin

package managedssh

// SystemRemoteLoginDenied always reports false: only macOS has a Remote Login ACL; elsewhere sshd's own configuration decides.
func SystemRemoteLoginDenied(string) bool { return false }
