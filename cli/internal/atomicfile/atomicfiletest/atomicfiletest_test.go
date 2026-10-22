package atomicfiletest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

func TestAtomicWriteIsOwnerOnlyAndAPlainFileIsNot(t *testing.T) {
	dir := shorttemp.Dir(t)
	private := filepath.Join(dir, "private")
	if err := atomicfile.Write(private, []byte("x"), atomicfile.CurrentOwnerOptions(0o600)); err != nil {
		t.Fatal(err)
	}
	RequireOwnerOnly(t, private, 0o600)

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkOwnerOnly(plain, 0o600); err == nil {
		t.Fatal("a plain world-readable file passed the owner-only check")
	}
}
