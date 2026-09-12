//go:build windows

package managedssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadHostPublicKeysRejectsHardLinkedFile(t *testing.T) {
	directory := t.TempDir()
	path := writeHostPublicKey(t, directory, "ssh_host_ed25519_key.pub", "ed25519")
	alias := filepath.Join(directory, "ssh_host_ed25519_alias.pub")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostPublicKeys([]string{path}, 0); err == nil {
		t.Fatal("hard-linked host public key was accepted")
	}
}

func TestReadHostPublicKeysRejectsRedirectedAncestor(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "real")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeHostPublicKey(t, directory, "ssh_host_ed25519_key.pub", "ed25519")
	if _, err := ReadHostPublicKeys([]string{path}, 0); err != nil {
		t.Fatalf("read direct host public key: %v", err)
	}
	alias := filepath.Join(root, "redirected")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", alias, directory).CombinedOutput(); err != nil {
		t.Fatalf("create directory junction: %v: %s", err, output)
	}
	redirected := filepath.Join(alias, filepath.Base(path))
	if _, err := ReadHostPublicKeys([]string{redirected}, 0); err == nil {
		t.Fatal("host public key through redirected ancestor was accepted")
	}
}
