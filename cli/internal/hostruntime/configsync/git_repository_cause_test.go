package configsync

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
)

func TestGitCheckoutFailuresRetainCauseAndRecover(t *testing.T) {
	bare, _ := transportFixture(t)
	repository := newTransportRepository(t, bare, "local", "")
	if err := os.Mkdir(repository.root, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := repository.Fetch(context.Background())
	if !errors.Is(err, ErrGitRepositoryInvalid) || !errors.Is(err, git.ErrRepositoryNotExists) {
		t.Fatal("invalid checkout lost its original Git cause")
	}
	if strings.Contains(err.Error(), repository.root) {
		t.Fatal("checkout path leaked into safe error")
	}
	if err := os.Remove(repository.root); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Fetch(context.Background()); err != nil {
		t.Fatal("fresh clone did not recover")
	}
	checkout, err := git.PlainOpen(repository.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.DeleteRemote("origin"); err != nil {
		t.Fatal(err)
	}
	_, err = repository.Fetch(context.Background())
	if !errors.Is(err, ErrGitRepositoryInvalid) || !errors.Is(err, git.ErrRemoteNotFound) {
		t.Fatal("missing remote lost its original Git cause")
	}
	if _, err := checkout.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Fetch(context.Background()); err != nil {
		t.Fatal("restoring the remote did not recover")
	}
}
