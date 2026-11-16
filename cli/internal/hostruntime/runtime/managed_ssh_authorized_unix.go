//go:build darwin || linux

package runtime

import "github.com/pinksaucepasta/paperboat/internal/managedssh"

// reconcilePlatformAuthorizedKeys writes the CA lines; the revoked list is enforced by the host bridge on Linux and
// macOS (every managed certificate is bound to its loopback connection), not by the system sshd.
func reconcilePlatformAuthorizedKeys(home string, ownerUID uint32, keys []string, principal string, _ []string) (bool, error) {
	result, err := managedssh.ReconcileAuthorizedKeys(home, ownerUID, keys, principal)
	return result.Changed, err
}
