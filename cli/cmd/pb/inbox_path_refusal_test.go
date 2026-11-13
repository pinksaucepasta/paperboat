package main

import (
	"errors"
	"strings"
	"testing"
)

func TestInboxPathNamesTheFolderAndTheCauseInsteadOfCouldNotFinish(t *testing.T) {
	cause := errors.New("inbox path is not writable: read-only file system")
	err := inboxPathRefusal("/mnt/ro", cause)
	var usage usageError
	if !errors.As(err, &usage) || !usage.refusal {
		t.Fatalf("want a refusal the user fixes, got %T %v", err, err)
	}
	for _, want := range []string{"/mnt/ro", "read-only file system", "pb inbox set"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q missing from %q", want, err.Error())
		}
	}
	if inboxPathRefusal("", cause) != cause {
		t.Fatal("an unreadable registration keeps its own error")
	}
}
