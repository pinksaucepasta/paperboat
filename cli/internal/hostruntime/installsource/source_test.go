package installsource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuppliedSourceBindsBytesAndUpdatePolicy(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, distribution := range []string{Custom, Official} {
		source, err := Inspect(path, "development", distribution)
		if err != nil {
			t.Fatal(err)
		}
		if source.AutomaticUpdates != (distribution == Official) {
			t.Fatal("incorrect update policy")
		}
		if err := source.Verify(path); err != nil {
			t.Fatal(err)
		}
		if distribution == Custom {
			source.AutomaticUpdates = true
			if source.Validate() == nil {
				t.Fatal("custom source enabled automatic replacements")
			}
		}
	}
	source, err := Inspect(path, "development", Custom)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	if source.Verify(path) == nil {
		t.Fatal("same-length changed binary accepted")
	}
	if source.Verify(filepath.Join(t.TempDir(), "missing")) == nil {
		t.Fatal("missing binary accepted")
	}
}
