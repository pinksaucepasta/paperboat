//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigServiceRuntimeRefusalNamesWhatIsWrong(t *testing.T) {
	root, err := os.MkdirTemp("", "cfgrt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	uid := os.Geteuid()
	if refusal := configServiceRuntimeRefusal(filepath.Join(root, "absent"), uid); refusal == "" {
		t.Fatal("a missing runtime directory was accepted")
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if refusal := configServiceRuntimeRefusal(root, uid); refusal == "" {
		t.Fatal("a group- and world-readable runtime directory was accepted")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if refusal := configServiceRuntimeRefusal(root, uid); refusal != "bus is not readable" {
		t.Fatalf("refusal %q, want the missing bus named", refusal)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if refusal := configServiceRuntimeRefusal(root, uid); refusal != "" {
		t.Fatalf("the user's own private runtime directory and bus were refused: %q", refusal)
	}
	if refusal := configServiceRuntimeRefusal(root, uid+1); refusal == "" {
		t.Fatal("another user's runtime directory was accepted")
	}
}
