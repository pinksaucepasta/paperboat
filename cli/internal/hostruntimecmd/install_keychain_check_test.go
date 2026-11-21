package hostruntimecmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// lockedKeychain answers like a locked macOS login keychain: `security` exits 36
// (errSecInteractionNotAllowed), which config reports as ErrCredentialStoreUnavailable.
type lockedKeychain struct{ config.FileSecretStore }

func (lockedKeychain) Get(string) (string, error) {
	return "", fmt.Errorf("macOS Keychain does not allow access from this session: %w", config.ErrCredentialStoreUnavailable)
}

func TestCheckDaemonSignInNamesLockedKeychain(t *testing.T) {
	const issuer = "https://api.example.test"
	dir := t.TempDir()
	files := config.FileSecretStore{Dir: filepath.Join(dir, "secrets")}
	store := config.ProfileStore{Path: dir, Secrets: files}
	if err := checkDaemonSignIn(store, issuer); err != nil {
		t.Fatalf("signed out: %v", err)
	}
	if err := store.Save(config.Profile{Issuer: issuer, CLIClientSessionID: "cls_1"}, config.Credential{AccessToken: "access", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := checkDaemonSignIn(store, issuer); err != nil {
		t.Fatalf("unlocked: %v", err)
	}
	store.Secrets = lockedKeychain{files}
	err := checkDaemonSignIn(store, issuer)
	var failure *CommandFailure // the type pb's renderer prints verbatim
	if !errors.As(err, &failure) || !errors.Is(err, config.ErrCredentialStoreUnavailable) || !strings.Contains(failure.Error(), "security unlock-keychain") {
		t.Fatalf("locked keychain: %v", err)
	}
}
