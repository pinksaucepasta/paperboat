package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type cyclicCredentialCause struct{ next error }

func (*cyclicCredentialCause) Error() string     { return "cyclic cause" }
func (err *cyclicCredentialCause) Unwrap() error { return err.next }

type injectedCredentialStore struct {
	getErr      error
	setErr      error
	deleteErr   error
	setCalls    int
	deleteCalls int
	values      map[string]string
}

func (store *injectedCredentialStore) Set(ref, value string) error {
	store.setCalls++
	if store.setErr != nil {
		return store.setErr
	}
	if store.values == nil {
		store.values = make(map[string]string)
	}
	store.values[ref] = value
	return nil
}

func (store *injectedCredentialStore) Get(ref string) (string, error) {
	if store.getErr != nil {
		return "", store.getErr
	}
	value, ok := store.values[ref]
	if !ok {
		return "", ErrSecretNotFound
	}
	return value, nil
}

func (store *injectedCredentialStore) Delete(ref string) error {
	store.deleteCalls++
	if store.deleteErr != nil {
		return store.deleteErr
	}
	delete(store.values, ref)
	return nil
}

func TestSafeConfigCauseHidesTextAndPreservesTypedCause(t *testing.T) {
	validation := safeConfigCause("credential file must be mode 0600", nil)
	if validation.Error() != "credential file must be mode 0600" || !isSafeConfigError(validation) || errors.Is(validation, os.ErrNotExist) {
		t.Fatalf("safe validation error = %T %v", validation, validation)
	}

	cause := &os.PathError{Op: "open", Path: "/private/credential-value", Err: syscall.EIO}
	err := safeConfigCause("credential file read failed", cause)
	if got := err.Error(); got != "credential file read failed" || strings.Contains(got, "private") {
		t.Fatalf("public error = %q", got)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr != cause || !errors.Is(err, syscall.EIO) {
		t.Fatalf("typed cause was lost: %T %v", err, err)
	}

	storeErr := credentialStoreFailure("credential store write failed", cause)
	if storeErr.Error() != "credential store write failed" || !errors.Is(storeErr, ErrCredentialStoreUnavailable) || !errors.As(storeErr, &pathErr) || !errors.Is(storeErr, syscall.EIO) {
		t.Fatalf("safe store error did not retain sentinel and cause: %v", storeErr)
	}
}

func TestCredentialAbsenceRequiresCompleteBoundedCause(t *testing.T) {
	if !credentialAbsenceOnly(ErrSecretNotFound) || !credentialAbsenceOnly(syscall.ENOENT) || !credentialAbsenceOnly(fmt.Errorf("read credential: %w", os.ErrNotExist)) {
		t.Fatal("ordinary missing credential was not recognized")
	}
	if !credentialAbsenceOnly(safeConfigCause("credential file is missing", os.ErrNotExist)) {
		t.Fatal("package-owned safe cause did not preserve pure absence")
	}
	if credentialAbsenceOnly(safeConfigCause("credential read failed", errors.Join(os.ErrNotExist, syscall.EIO))) {
		t.Fatal("package-owned safe cause hid a mixed I/O failure")
	}
	if credentialAbsenceOnly(credentialStoreFailure("credential store unavailable", os.ErrNotExist)) {
		t.Fatal("independent unavailable marker was treated as absence")
	}
	if !credentialAbsenceOnly(errors.Join(ErrSecretNotFound, os.ErrNotExist)) {
		t.Fatal("joined absence-only causes were not recognized")
	}
	if credentialAbsenceOnly(errors.Join(ErrSecretNotFound, syscall.EIO)) {
		t.Fatal("mixed missing and operational causes were treated as absence")
	}

	cycle := &cyclicCredentialCause{}
	cycle.next = cycle
	if credentialAbsenceOnly(errors.Join(ErrSecretNotFound, cycle)) {
		t.Fatal("cyclic cause was treated as absence")
	}
	tooDeep := error(ErrSecretNotFound)
	for range 16 {
		tooDeep = fmt.Errorf("wrap: %w", tooDeep)
	}
	if credentialAbsenceOnly(tooDeep) {
		t.Fatal("cause chain beyond the inspection bound was treated as absence")
	}
}

func TestIssuerNormalizationRejectsCredentialsAndHidesMalformedInput(t *testing.T) {
	const secretIssuer = "https://alice:issuer-secret@api.example.test"
	if _, err := NormalizeIssuer(secretIssuer); err == nil || strings.Contains(err.Error(), "issuer-secret") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("credential-bearing issuer error = %v", err)
	}

	const malformed = "https://api.example.test/%issuer-secret%"
	_, err := NormalizeIssuer(malformed)
	if err == nil || err.Error() != "invalid Paperboat server URL" || strings.Contains(err.Error(), "issuer-secret") {
		t.Fatalf("malformed issuer error = %v", err)
	}
	var parseErr *url.Error
	if !errors.As(err, &parseErr) {
		t.Fatalf("URL parser cause was lost: %T %v", err, err)
	}
}

func TestServerURLNormalizationRetainsParserCauseWithoutEcho(t *testing.T) {
	const malformed = "https://api.example.test/%api-key-secret%"
	_, err := NormalizeServerURL(malformed)
	if err == nil || err.Error() != "invalid Paperboat server URL" || strings.Contains(err.Error(), "api-key-secret") {
		t.Fatalf("malformed server URL error = %v", err)
	}
	var parseErr *url.Error
	if !errors.As(err, &parseErr) {
		t.Fatalf("URL parser cause was lost: %T %v", err, err)
	}
}

func TestCredentialAbsenceConsumersRejectMixedOperationalCause(t *testing.T) {
	const issuer, accountID, endpointID = "https://api.example.test", "account_1", "cli_1"
	missingAndIO := errors.Join(ErrSecretNotFound, &os.PathError{Op: "read", Path: "/private/credential-value", Err: syscall.EIO})

	newStore := func() (*ProfileStore, *injectedCredentialStore) {
		secrets := &injectedCredentialStore{getErr: missingAndIO}
		return &ProfileStore{Path: t.TempDir(), Secrets: secrets}, secrets
	}
	check := func(name string, err error, secrets *injectedCredentialStore) {
		t.Helper()
		if err == nil || !errors.Is(err, syscall.EIO) {
			t.Fatalf("%s error = %v", name, err)
		}
		if strings.Contains(err.Error(), "/private/credential-value") || secrets.setCalls != 0 {
			t.Fatalf("%s exposed provider details or attempted replacement: err=%v writes=%d", name, err, secrets.setCalls)
		}
	}

	store, secrets := newStore()
	if _, err := store.ManagedSSHIdentity(issuer, endpointID); err == nil {
		t.Fatal("managed SSH identity treated a mixed failure as absent")
	} else {
		check("managed SSH identity", err, secrets)
	}
	store, secrets = newStore()
	if _, err := store.NetworkFingerprintSecret(); err == nil {
		t.Fatal("network fingerprint secret treated a mixed failure as absent")
	} else {
		check("network fingerprint secret", err, secrets)
	}
	store, secrets = newStore()
	if _, err := store.PeerEndpointKeys(issuer, accountID, endpointID); err == nil {
		t.Fatal("peer endpoint key treated a mixed failure as absent")
	} else {
		check("peer endpoint key", err, secrets)
	}
	store, secrets = newStore()
	if err := store.UpdatePeerNetworkState(issuer, accountID, endpointID, func(*PeerNetworkState) error {
		t.Fatal("peer network update ran after a mixed read failure")
		return nil
	}); err == nil {
		t.Fatal("peer network state treated a mixed failure as absent")
	} else {
		check("peer network state", err, secrets)
	}
	store, secrets = newStore()
	callbackCalled := false
	err := store.WithBrowserLogin(issuer, func(*BrowserLoginState, func() error) error {
		callbackCalled = true
		return nil
	})
	if callbackCalled {
		t.Fatal("browser login callback ran after a mixed read failure")
	}
	check("browser login state", err, secrets)
}

func TestPeerNetworkStateWriteFailureRetainsCauseWithStaticText(t *testing.T) {
	const issuer, accountID, endpointID = "https://api.example.test", "account_1", "cli_1"
	cause := &os.PathError{Op: "write", Path: "/private/peer-network-key", Err: syscall.EIO}
	secrets := &injectedCredentialStore{getErr: ErrSecretNotFound, setErr: cause}
	store := ProfileStore{Path: t.TempDir(), Secrets: secrets}
	err := store.UpdatePeerNetworkState(issuer, accountID, endpointID, func(state *PeerNetworkState) error {
		state.PrivateKey = make([]byte, 32)
		state.KeyGeneration = 1
		state.Generation = 1
		state.ConfigHash = strings.Repeat("a", 64)
		return nil
	})
	if err == nil || err.Error() != "peer network custody could not be stored" || !errors.Is(err, ErrCredentialStoreUnavailable) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("peer network write failure = %v", err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr != cause || strings.Contains(err.Error(), "/private/peer-network-key") {
		t.Fatalf("peer network write cause was exposed or lost: %T %v", err, err)
	}
}

func TestSharedLockErrorHidesLocalPathAndPreservesFilesystemCause(t *testing.T) {
	root := t.TempDir()
	privatePath := filepath.Join(root, "private-profile-location")
	if err := os.WriteFile(privatePath, []byte("block parent creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := newSharedLock(filepath.Join(privatePath, "profile.lock")).Lock()
	if err == nil || strings.Contains(err.Error(), privatePath) {
		t.Fatalf("shared lock error = %v", err)
	}
}
