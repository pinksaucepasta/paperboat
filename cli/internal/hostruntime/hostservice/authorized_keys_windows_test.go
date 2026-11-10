//go:build windows

package hostservice

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"golang.org/x/crypto/ssh"
)

func TestWindowsAuthorizedKeysOwnsFixedDedicatedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ssh")
	reconciler, err := newWindowsAuthorizedKeys(root)
	if err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	changed, err := reconciler.ReconcileAuthorizedKeys(context.Background(), []string{line})
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	path := filepath.Join(root, "authorized_keys", "paperboat")
	body, err := os.ReadFile(path)
	if err != nil || string(body) != line+"\n" {
		t.Fatalf("authorized keys=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".ssh")); !os.IsNotExist(err) {
		t.Fatalf("unexpected user SSH path: %v", err)
	}
}

func TestWindowsAuthorizedKeysEndsSessionsOnlyWhenAKeyLeaves(t *testing.T) {
	reconciler, err := newWindowsAuthorizedKeys(filepath.Join(t.TempDir(), "ssh"))
	if err != nil {
		t.Fatal(err)
	}
	var ended []string
	reconciler.instance = "u-test"
	reconciler.terminate = func(instance, _ string, removed []string) (windowsopenssh.TerminateResult, error) {
		ended = append(ended, instance+":"+strings.Join(removed, ","))
		return windowsopenssh.TerminateResult{}, nil
	}
	newKey := func() string {
		public, _, _ := ed25519.GenerateKey(rand.Reader)
		key, _ := ssh.NewPublicKey(public)
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	}
	a, b := newKey(), newKey()
	for _, step := range []struct {
		keys []string
		want int // total terminations so far
	}{{[]string{a}, 0}, {[]string{a, b}, 0}, {[]string{a, b}, 0}, {[]string{b}, 1}, {nil, 2}} {
		if _, err := reconciler.ReconcileAuthorizedKeys(context.Background(), step.keys); err != nil {
			t.Fatal(err)
		}
		if len(ended) != step.want {
			t.Fatalf("after %d keys ended=%v, want %d", len(step.keys), ended, step.want)
		}
	}
	if want := "u-test:" + fingerprintOf(a); ended[0] != want {
		t.Fatalf("ended %q, want only the removed key %q", ended[0], want)
	}
}

func TestWindowsAuthorizedKeysRetriesAFailedSessionEnd(t *testing.T) {
	reconciler, err := newWindowsAuthorizedKeys(filepath.Join(t.TempDir(), "ssh"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, fail := 0, true
	reconciler.instance = "u-test"
	reconciler.terminate = func(string, string, []string) (windowsopenssh.TerminateResult, error) {
		attempts++
		if fail {
			return windowsopenssh.TerminateResult{}, os.ErrPermission
		}
		return windowsopenssh.TerminateResult{}, nil
	}
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(public)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	for _, keys := range [][]string{{line}, nil} {
		reconciler.ReconcileAuthorizedKeys(context.Background(), keys)
	}
	if attempts != 1 {
		t.Fatalf("attempts after the revoke = %d", attempts)
	}
	fail = false
	if _, err := reconciler.ReconcileAuthorizedKeys(context.Background(), nil); err != nil || attempts != 2 {
		t.Fatalf("the unchanged refresh did not retry: attempts=%d err=%v", attempts, err)
	}
	reconciler.ReconcileAuthorizedKeys(context.Background(), nil)
	if attempts != 2 {
		t.Fatalf("a finished duty ran again: %d", attempts)
	}
}

func fingerprintOf(line string) string {
	key, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(line))
	return ssh.FingerprintSHA256(key)
}

func TestWindowsAuthorizedKeysLogsTheEndAllFallback(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(previous)
	reconciler, err := newWindowsAuthorizedKeys(filepath.Join(t.TempDir(), "ssh"))
	if err != nil {
		t.Fatal(err)
	}
	reconciler.instance = "u-test"
	reconciler.terminate = func(string, string, []string) (windowsopenssh.TerminateResult, error) {
		return windowsopenssh.TerminateResult{Ended: 3, Fallback: "the SSH service log could not be read"}, nil
	}
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(public)
	for _, keys := range [][]string{{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}, nil} {
		if _, err := reconciler.ReconcileAuthorizedKeys(context.Background(), keys); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(logged.String(), "ended every session") || !strings.Contains(logged.String(), "service log could not be read") {
		t.Fatalf("the fallback was not logged with its reason: %q", logged.String())
	}
}
