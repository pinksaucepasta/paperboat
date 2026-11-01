//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

func TestCarrierDescriptorPromotionPreservesTheServingSourceUntilCutover(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected root-owned staging")
	}
	parent := "/var/lib"
	if runtime.GOOS == "darwin" {
		parent = "/Library"
	}
	root, err := os.MkdirTemp(parent, "paperboat-carrier-stage-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	current := filepath.Join(root, "config.json")
	staged := current + ".next"
	old, next := []byte(`{"source":"serving"}`), []byte(`{"source":"verified-candidate"}`)
	for _, v := range []struct {
		path string
		body []byte
	}{{current, old}, {staged, next}} {
		if err := atomicfile.Write(v.path, v.body, atomicfile.Options{Mode: 0640, OwnerUID: 0, OwnerGID: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if body, err := os.ReadFile(current); err != nil || !bytes.Equal(body, old) {
		t.Fatal("staging changed the serving source")
	}
	// An unprotected staged descriptor must not replace the active identity.
	if err := os.Chmod(staged, 0660); err != nil {
		t.Fatal(err)
	}
	if err := promoteCarrierConfig(staged); err == nil {
		t.Fatal("writable staged descriptor was accepted")
	}
	if body, _ := os.ReadFile(current); !bytes.Equal(body, old) {
		t.Fatal("rejected staging changed the serving source")
	}
	if err := os.Chmod(staged, 0640); err != nil {
		t.Fatal(err)
	}
	if err := (&unixCarrierUpgrade{stagedConfig: staged}).activate(); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(current); err != nil || !bytes.Equal(body, next) {
		t.Fatal("candidate descriptor not atomically promoted")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("promotion left staging behind")
	}
}
