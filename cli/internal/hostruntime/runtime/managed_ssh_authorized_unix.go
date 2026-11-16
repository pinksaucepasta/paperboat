//go:build darwin || linux

package runtime

import "github.com/pinksaucepasta/paperboat/internal/managedssh"

func reconcilePlatformAuthorizedKeys(home string, ownerUID uint32, keys []string, principal string) (bool, error) {
	result, err := managedssh.ReconcileAuthorizedKeys(home, ownerUID, keys, principal)
	return result.Changed, err
}
