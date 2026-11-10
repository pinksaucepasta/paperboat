package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

func TestLoginEnrollmentTokenAcceptedByBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment-token")
	const token = "0123456789ABCDEFGHIJKLMNOP"
	if err := writeLoginEnrollmentToken(path, token); err != nil {
		t.Fatal(err)
	}
	got, err := bootstrap.ReadEnrollmentTokenFile(path)
	if err != nil {
		t.Fatalf("browser login token refused by pairing: %v", err)
	}
	if got != token {
		t.Fatal("pairing did not read the browser login token")
	}
	if err := bootstrap.ConsumeEnrollmentTokenFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pairing did not remove the consumed browser login token")
	}
}
