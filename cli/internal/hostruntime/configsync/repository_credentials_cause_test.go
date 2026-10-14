package configsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

func TestRepositoryCredentialProfileFailuresKeepCausesAndHidePaths(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}

	_, err := LoadRepositoryCredentialProfile(root, "missing-profile")
	if !errors.Is(err, ErrRepositoryCredentials) || !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), root) {
		t.Fatalf("missing profile lost its credential classification/cause or exposed its path: %v", err)
	}

	path, err := credentialProfilePath(root, "malformed-profile")
	if err != nil {
		t.Fatal(err)
	}
	const canary = "PRIVATE_REPOSITORY_PASSWORD_CANARY"
	if err := os.WriteFile(path, []byte(`{"password":"`+canary+`",}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadRepositoryCredentialProfile(root, "malformed-profile")
	var syntaxErr *json.SyntaxError
	if !errors.Is(err, ErrRepositoryCredentials) || !errors.As(err, &syntaxErr) || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), path) {
		t.Fatalf("malformed profile lost its typed cause/classification or exposed private content: %v", err)
	}
}

func TestRepositoryCredentialReferenceClassificationRequiresOnlyExpectedCauses(t *testing.T) {
	missing := repositoryCredentialReferenceFailure(os.ErrNotExist)
	if !errors.Is(missing, ErrRepositoryCredentials) || errors.Is(missing, ErrRepositoryUnavailable) || !errors.Is(missing, os.ErrNotExist) {
		t.Fatalf("pure missing reference was not classified as credential configuration: %v", missing)
	}

	const privatePath = "/home/private/.config/paperboat/key"
	mixedCause := errors.Join(fmt.Errorf("missing reference: %w", os.ErrNotExist), &os.PathError{Op: "read", Path: privatePath, Err: syscall.EIO})
	mixed := repositoryCredentialReferenceFailure(mixedCause)
	var pathErr *os.PathError
	if !errors.Is(mixed, ErrRepositoryUnavailable) || errors.Is(mixed, ErrRepositoryCredentials) || !errors.Is(mixed, os.ErrNotExist) || !errors.Is(mixed, syscall.EIO) || !errors.As(mixed, &pathErr) {
		t.Fatalf("mixed reference failure was misclassified or lost causes: %v", mixed)
	}
	if strings.Contains(mixed.Error(), privatePath) || strings.Contains(mixed.Error(), "missing reference") {
		t.Fatalf("mixed reference failure exposed private cause: %q", mixed.Error())
	}

	if repositoryOnlyExpectedCredentialReferenceCause(&cyclicRepositoryReferenceError{}) {
		t.Fatal("cyclic reference cause was classified as a user credential mistake")
	}
}

type cyclicRepositoryReferenceError struct{}

func (*cyclicRepositoryReferenceError) Error() string   { return "cyclic reference failure" }
func (e *cyclicRepositoryReferenceError) Unwrap() error { return e }

func TestRepositoryCredentialStorageFailuresAreUnavailableWithOriginalCause(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}

	profilePath, err := credentialProfilePath(root, "blocked-write")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profilePath, 0o700); err != nil {
		t.Fatal(err)
	}
	err = SaveRepositoryCredentialProfile(root, "blocked-write", RepositoryCredentialProfile{Transport: "https", Auth: "anonymous"})
	var atomicErr *atomicfile.Error
	if !errors.Is(err, ErrRepositoryUnavailable) || errors.Is(err, ErrRepositoryCredentials) || !errors.As(err, &atomicErr) || strings.Contains(err.Error(), profilePath) {
		t.Fatalf("profile write failure lost storage classification/cause or exposed its path: %v", err)
	}

	deletePath, err := credentialProfilePath(root, "blocked-delete")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(deletePath, 0o700); err != nil {
		t.Fatal(err)
	}
	const canary = "PRIVATE_REPOSITORY_FILE_CANARY"
	if err := os.WriteFile(filepath.Join(deletePath, "private"), []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	err = DeleteRepositoryCredentialProfile(root, "blocked-delete")
	var pathErr *os.PathError
	if !errors.Is(err, ErrRepositoryUnavailable) || errors.Is(err, ErrRepositoryCredentials) || !errors.As(err, &pathErr) || strings.Contains(err.Error(), deletePath) || strings.Contains(err.Error(), canary) {
		t.Fatalf("profile delete failure lost storage classification/cause or exposed private data: %v", err)
	}
}

func TestSaveRepositoryCredentialProfilePreservesRootFilesystemFailure(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(blocker, "credentials")
	err := SaveRepositoryCredentialProfile(root, "repository", RepositoryCredentialProfile{Transport: "https", Auth: "anonymous"})
	if !errors.Is(err, ErrRepositoryUnavailable) || errors.Is(err, ErrRepositoryCredentials) || !errors.Is(err, syscall.ENOTDIR) || strings.Contains(err.Error(), blocker) {
		t.Fatalf("profile root failure lost its operational cause/classification or exposed its path: unavailable=%t credentials=%t not_directory=%t path_exposed=%t", errors.Is(err, ErrRepositoryUnavailable), errors.Is(err, ErrRepositoryCredentials), errors.Is(err, syscall.ENOTDIR), strings.Contains(err.Error(), blocker))
	}
}

func TestRepositoryReferenceMissingCauseKeepsCredentialClassification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-reference")
	_, err := readRepositoryReference(path, true)
	var pathErr *os.PathError
	if !errors.Is(err, ErrRepositoryCredentials) || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) || strings.Contains(err.Error(), path) {
		t.Fatalf("missing credential reference lost its typed cause/classification or exposed its path: %v", err)
	}
}

func TestRepositoryRootAndSharedReadFailuresKeepFilesystemCause(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(blocker, "cache")
	err := EnsureRepositoryCredentialRoot(root)
	var pathErr *os.PathError
	if !errors.Is(err, ErrRepositoryUnavailable) || !errors.As(err, &pathErr) || strings.Contains(err.Error(), blocker) {
		t.Fatalf("root setup lost its filesystem cause or exposed its path: %v", err)
	}

	access := RepositoryReadAccess{RepositoryID: "repo", CloneURL: filepath.Join(base, "unused.git"), Branch: "main", Transport: "local", Capability: "repository_contents_read", ExpiresAt: time.Now().Add(time.Minute)}
	_, _, err = ReadSharedRepositoryConfig(context.Background(), root, "", access, DefaultSourceConfigLimits())
	if !errors.Is(err, ErrRepositoryUnavailable) || !errors.As(err, &pathErr) || strings.Contains(err.Error(), blocker) {
		t.Fatalf("shared read lost storage cause or exposed its path: %v", err)
	}

	staging, stagingErr := newPrivateRepositoryTemporaryDirectory(root, ".test-")
	if staging != "" || !errors.Is(stagingErr, ErrRepositoryUnavailable) || strings.Contains(stagingErr.Error(), blocker) {
		t.Fatalf("staging creation lost its safe storage classification or exposed its path: %v", stagingErr)
	}
	if !errors.As(stagingErr, &pathErr) && !errors.Is(stagingErr, os.ErrNotExist) && !errors.Is(stagingErr, syscall.ENOTDIR) {
		t.Fatalf("staging failure lost the original filesystem cause (path_error=%t not_found=%t not_directory=%t)", errors.As(stagingErr, &pathErr), errors.Is(stagingErr, os.ErrNotExist), errors.Is(stagingErr, syscall.ENOTDIR))
	}
}

func TestRepositoryCleanupFailureDominatesCredentialRejection(t *testing.T) {
	primary := repositoryCredentialsFailure(os.ErrNotExist)
	operational := repositoryUnavailableFailure(fmt.Errorf("private repository cleanup failure: %w", syscall.EIO))
	err := repositoryCleanupFailure(primary, operational)
	if !errors.Is(err, ErrRepositoryUnavailable) || !errors.Is(err, ErrRepositoryCredentials) || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("joined cleanup failure lost classification or cause: %v", err)
	}
	if strings.Contains(err.Error(), "private repository") || strings.Contains(err.Error(), "cleanup failure") {
		t.Fatalf("joined cleanup failure exposed private cause: %q", err)
	}
}
