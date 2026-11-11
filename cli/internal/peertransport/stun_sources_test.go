package peertransport_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// F21-29: STUN is asked only of the relays, so no production file in the peer transport (or the code that
// feeds it) names a STUN server of its own: no stun: URL, no stun host name, no STUN port literal. A public
// endpoint comes from a relay's STUN answer, like Tailscale's DERP map (netcheck probes the map's nodes only).
func TestNoProductionFileNamesAStunServer(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)"stun[0-9]*:|\bstun[0-9]*\.[a-z0-9-]+\.[a-z]{2,}|:3478\b|:19302\b`)
	for _, root := range []string{".", "../daemoncmd", "../hostruntime/native", "../../../shared/peernetwork"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if match := pattern.Find(source); match != nil {
				t.Errorf("%s names a STUN server %q: STUN is asked only of the relays", path, match)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
