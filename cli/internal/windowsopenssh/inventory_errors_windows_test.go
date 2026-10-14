//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestInventoryQueryFailureKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private inventory path C:\\Users\\Alice\\OpenSSH\\sshd.exe"
	cause := errors.New("inventory query failed")
	_, err := collectInventory(context.Background(), Config{Runner: verificationOutputRunner{
		output: []byte(privateTranscript), err: cause,
	}})
	if !errors.Is(err, cause) {
		t.Fatalf("inventory error = %v, want runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("inventory error exposed command output: %v", err)
	}
}

func TestSystemWingetInventoryFailureKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private package cache C:\\Users\\Alice\\AppData\\Local\\Temp"
	cause := errors.New("WinGet inventory failed")
	runner := &queuedRunner{replies: []runnerReply{
		{output: []byte(approvedWinGetModuleVersion)},
		{output: []byte(privateTranscript), err: cause},
	}}
	_, _, err := inventoryWithSystemWinget(context.Background(), runner)
	if !errors.Is(err, ErrInstallerUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("system inventory error = %v, want installer classification and runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("system inventory error exposed command output: %v", err)
	}
}
