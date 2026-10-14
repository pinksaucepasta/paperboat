package installsource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sourcePrivateError struct{}

func (*sourcePrivateError) Error() string { panic("private source error formatted") }

func TestDigestCloseFailureRetainsCauseClearsIdentityAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-source")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, primary := range []error{nil, &sourcePrivateError{}} {
		length, digest := int64(12), strings.Repeat("a", 64)
		resultErr := primary
		closeDigestFile(file, &length, &digest, &resultErr)()
		if length != 0 || digest != "" || !errors.Is(resultErr, os.ErrClosed) || !errors.Is(resultErr, ErrInvalid) {
			t.Fatal("close failure escaped as usable identity")
		}
		if primary != nil && !errors.Is(resultErr, primary) {
			t.Fatal("primary error lost")
		}
		if strings.Contains(resultErr.Error(), "private-source") {
			t.Fatal("private path exposed")
		}
	}
	if err := os.WriteFile(path, []byte("healthy bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	length, digest, err := digestFile(path)
	if err != nil || length != 13 || len(digest) != 64 {
		t.Fatal("fresh hash did not recover")
	}
}
