//go:build !darwin && !linux

package managedssh

import "errors"

// ErrAuthorizedKeysConflict: an SSH file Paperboat must read or write is not in the shape it can safely change.
var ErrAuthorizedKeysConflict = errors.New("managed SSH authorized_keys reconciliation conflict")

type AuthorizedKeysResult struct {
	Changed bool
	Count   int
}

// ReconcileAuthorizedKeys: Windows hosts trust the user CA in PaperboatSshd's own file (windowsopenssh), never in a
// user's authorized_keys.
func ReconcileAuthorizedKeys(string, uint32, []string, string) (AuthorizedKeysResult, error) {
	return AuthorizedKeysResult{}, errors.New("managed SSH authorized_keys reconciliation is unsupported on this platform")
}
