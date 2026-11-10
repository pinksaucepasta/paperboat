//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
	"testing"
)

func TestKeepInstallFailureOwnerKeepsTheAuthoredFailureAsTheOnlyOwner(t *testing.T) {
	authored := runtimeFailure("installing the supplied Paperboat executable failed (its reason is printed above); if administrator access was declined, approve it and rerun `pb install`", errors.New("exit status 1"))
	inventoryErr := errors.New("sudo: a terminal is required")
	got := KeepInstallFailureOwner(authored, inventoryErr)
	failure, ok := got.(*CommandFailure)
	if !ok || failure.Error() != authored.Error() {
		t.Fatalf("the authored refusal is no longer the owner: %T %v", got, got)
	}
	if !errors.Is(got, inventoryErr) {
		t.Fatal("the inventory failure was dropped from the causes")
	}
	if KeepInstallFailureOwner(authored, nil) != authored {
		t.Fatal("a successful inventory changed the failure")
	}
	sentinel := errors.New("typed sentinel")
	if joined := KeepInstallFailureOwner(sentinel, inventoryErr); !errors.Is(joined, sentinel) || !errors.Is(joined, inventoryErr) {
		t.Fatal("a non-authored failure lost a cause")
	}
}
