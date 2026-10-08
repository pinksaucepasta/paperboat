package preferences

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreferenceValidationKeepsCauseAndDoesNotExposePrivateInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"PRIVATE_PREFERENCE_VALUE":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	var invalid *InvalidError
	if !errors.As(err, &invalid) || !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "PRIVATE_PREFERENCE_VALUE") {
		t.Fatal("preferences validation lost its safe typed boundary")
	}
	if invalid.Unwrap() == nil {
		t.Fatal("preferences parser cause was discarded")
	}
	if err := os.WriteFile(path, []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("truncated preferences were accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":1} ?`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatal("trailing JSON parse cause was discarded")
	}
}

func TestPreferenceFilesystemFailureKeepsOriginalCauseAndRecovers(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private-parent")
	if err := os.WriteFile(parent, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "preferences.json")
	_, err := Load(path)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || errors.Is(err, ErrInvalid) || pathErr.Op != "lstat" || pathErr.Err == nil {
		t.Fatal("filesystem outage became invalid preferences")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path)
	if err != nil || doc.Version != 1 {
		t.Fatal("preferences did not recover after storage repair")
	}
}
