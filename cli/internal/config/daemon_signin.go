package config

import (
	"errors"
	"os"
	"os/exec"
	"runtime"

	"golang.org/x/term"
)

// DaemonSignInHelp says what to do when the daemon cannot read its sign-in from a locked login keychain.
const DaemonSignInHelp = "reading this Mac's Paperboat sign-in from the login keychain failed, so the daemon could not start; " +
	"unlock it with `security unlock-keychain ~/Library/Keychains/login.keychain-db` (or sign in on the Mac's screen) and run the command again"

// CheckDaemonSignIn reads the sign-in the user daemon reads before it reports ready. On macOS that is the
// login keychain: locked, the daemon could not start and install rolled back with only "context deadline
// exceeded" (apple, dev-025cef983). It returns an ErrCredentialStoreUnavailable error only; a signed-out or
// otherwise broken profile is the daemon's to report, as before.
func CheckDaemonSignIn(cfg *Config, issuer string) error {
	if runtime.GOOS != "darwin" || cfg.Auth.AllowFileFallback {
		return nil
	}
	store, err := ProfileStoreFor(cfg)
	if err != nil {
		return err
	}
	return checkSignInUnlocking(store, issuer)
}

func checkSignInUnlocking(store ProfileStore, issuer string) error {
	// Known locked at a terminal: unlock first, since a read would wait on a dialog nobody may see (SSH).
	if stdinIsTerminal() && keychainLocked() {
		if unlockErr := unlockKeychain(); unlockErr != nil {
			return errors.Join(credentialStoreFailure("the macOS login keychain is locked", errKeychainLocked), unlockErr)
		}
	}
	err := checkSignIn(store, issuer)
	if err == nil || !stdinIsTerminal() {
		return err
	}
	// User decision 2026-11-21: at a terminal, unlock in place. `security` asks for the password on the
	// terminal itself; pb never sees it.
	if unlockErr := unlockKeychain(); unlockErr != nil {
		return errors.Join(err, unlockErr)
	}
	return checkSignIn(store, issuer)
}

var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

var unlockKeychain = func() error {
	command := exec.Command("/usr/bin/security", "unlock-keychain")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func checkSignIn(store ProfileStore, issuer string) error {
	if _, err := store.CredentialFor(issuer); errors.Is(err, ErrCredentialStoreUnavailable) {
		return err
	}
	return nil
}
