package hostruntimecmd

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// checkDaemonSignIn reads the sign-in the user daemon reads before it reports ready. On macOS that is the
// login keychain: locked, the daemon sat on an unlock dialog until install's readiness deadline and the
// install rolled back with only "context deadline exceeded" (apple, dev-025cef983). Asking here first fails
// before anything is replaced and says what to do.
func checkDaemonSignIn(store config.ProfileStore, server string) error {
	if _, err := store.CredentialFor(server); errors.Is(err, config.ErrCredentialStoreUnavailable) {
		return runtimeFailure("reading this Mac's Paperboat sign-in from the login keychain failed, so the daemon could not start; "+
			"unlock it with `security unlock-keychain ~/Library/Keychains/login.keychain-db` (or sign in on the Mac's screen) "+
			"and run pb install again", err)
	}
	return nil // signed out or other profile problems are the daemon's to report, as before
}
