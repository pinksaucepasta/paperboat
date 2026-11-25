//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryQueryFailureKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private inventory path C:\\Users\\Alice\\OpenSSH\\sshd.exe"
	cause := errors.New("inventory query failed")
	_, err := collectInventory(context.Background(), Config{Runner: verificationOutputRunner{
		output: []byte(privateTranscript), err: cause,
	}})
	if !errors.Is(err, cause) || !Unobtainable(err) {
		t.Fatalf("inventory error = %v, want unobtainable with the runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("inventory error exposed command output: %v", err)
	}
}

func TestUndecodableInventoryIsUnobtainable(t *testing.T) {
	_, err := collectInventory(context.Background(), Config{Runner: verificationOutputRunner{output: []byte("WARNING: not JSON")}})
	if !Unobtainable(err) {
		t.Fatalf("inventory error = %v, want unobtainable", err)
	}
}

func TestSharedStateWithoutInventoryIsUnobtainable(t *testing.T) {
	root := t.TempDir()
	_, err := PrepareSharedState(context.Background(), Config{Platform: "windows", InstallRoot: filepath.Join(root, "OpenSSH"), StateRoot: filepath.Join(root, "state"), ApprovedVersion: ApprovedVersion, ExpectedPublisher: "Microsoft", Port: 38222, Runner: noPackageClientRunner{}})
	if !Unobtainable(err) {
		t.Fatalf("shared state error = %v, want unobtainable", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "state")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("skipped OpenSSH left state behind: %v", statErr)
	}
}
