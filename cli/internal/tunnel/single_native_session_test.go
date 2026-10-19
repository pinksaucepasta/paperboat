package tunnel

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI-role peer identity may hold ONE native session at a time: relays reject a second
// registration on the same identity ("relay authorization rejected"). The daemon owns it, so
// no other process may build its own PeerTerminalTunnel (and with it a native runtime).
func TestOnlyTheDaemonBuildsAPeerTerminalTunnel(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{
		filepath.Join("internal", "daemoncmd", "main.go"):       true,
		filepath.Join("internal", "tunnel", "peer_terminal.go"): true, // the constructor itself
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "NewPeerTerminalTunnel(") && !allowed[relative] {
			t.Errorf("%s builds its own PeerTerminalTunnel; route the call through the local daemon (localapi) instead", relative)
		}
		if strings.Contains(string(data), "newCLINativeRuntime(") && relative != filepath.Join("internal", "tunnel", "native_runtime.go") {
			t.Errorf("%s opens a CLI native runtime outside PeerTerminalTunnel", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
