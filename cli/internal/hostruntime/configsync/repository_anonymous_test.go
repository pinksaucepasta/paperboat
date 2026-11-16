package configsync

import (
	"context"
	"errors"
	"testing"
)

// With no saved credential profile an HTTPS repository is read anonymously (a public repository named by a
// recipe's [config] needs no per-machine setup); plain HTTP and SSH still need a saved profile.
func TestHTTPSWithoutAProfileIsReadAnonymously(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	access, err := resolveRepositoryTransportAccess(context.Background(), root, RepositoryAccess{RepositoryID: "repo", Transport: "https", CloneURL: "https://git.example.test/public.git"})
	if err != nil || access.auth != nil {
		t.Fatalf("https: auth=%v err=%v", access.auth, err)
	}
	for _, transport := range []string{"http", "ssh"} {
		if _, err := resolveRepositoryTransportAccess(context.Background(), root, RepositoryAccess{RepositoryID: "repo", Transport: transport, CloneURL: transport + "://git.example.test/r.git"}); !errors.Is(err, ErrRepositoryCredentials) {
			t.Fatalf("%s without a profile: %v", transport, err)
		}
	}
}
