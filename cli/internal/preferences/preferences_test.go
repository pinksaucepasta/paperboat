package preferences

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveStrictAndPreservesExplicitEmptyPanels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	doc := Default()
	doc.TUI.PreviewPanels = []string{}
	if err := Save(path, doc); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TUI.PreviewPanels == nil {
		t.Fatal("explicit empty panels collapsed to nil")
	}
	clone := FromContext(WithContext(context.Background(), loaded))
	if clone.TUI.PreviewPanels == nil {
		t.Fatal("context clone collapsed explicit empty panels")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestSaveIfUnchangedAndSymlinkProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	revision, err := Revision(path)
	if err != nil || revision != "missing" {
		t.Fatalf("revision=%q err=%v", revision, err)
	}
	if err := SaveIfUnchanged(path, Default(), revision); err != nil {
		t.Fatal(err)
	}
	if err := SaveIfUnchanged(path, Default(), revision); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale save err=%v", err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if err := Save(link, Default()); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestRevisionRejectsOversizedAndSymlinkFiles(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large.json")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(maxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := Revision(large); err == nil {
		t.Fatal("oversized revision input accepted")
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if _, err := Revision(link); err == nil {
		t.Fatal("symlink revision input accepted")
	}
}

func TestValidateRejectsUnsafeCustomization(t *testing.T) {
	doc := Default()
	doc.Shortcuts = map[string]Shortcut{"bad": {Command: []string{"ssh"}, Args: []string{"{args}-suffix"}}}
	if err := Validate(doc); err == nil {
		t.Fatal("embedded {args} accepted")
	}
	doc = Default()
	doc.TUI.Keys = map[string]string{"up": "x"}
	if err := Validate(doc); err == nil {
		t.Fatal("printable navigation key accepted")
	}
	doc = Document{Version: 1, TUI: TUI{Keys: map[string]string{"up": "down"}}}
	if err := Validate(doc); err == nil {
		t.Fatal("partial key collision accepted")
	}
	doc = Default()
	doc.TUI.Accent = "red"
	if err := Validate(doc); err == nil {
		t.Fatal("named accent accepted")
	}
	doc.TUI.Accent = "255"
	if err := Validate(doc); err != nil {
		t.Fatalf("numeric accent rejected: %v", err)
	}
}

func TestCustomizationCatalogsAndDuplicates(t *testing.T) {
	doc := Default()
	doc.TUI.HomeOrder = []string{"machines", "environment-variables", "team", "commands", "customize"}
	if err := Validate(doc); err != nil {
		t.Fatal(err)
	}
	doc.TUI.HomeOrder = []string{"machines", "machines"}
	if err := Validate(doc); err == nil {
		t.Fatal("duplicate home item accepted")
	}
	doc = Default()
	doc.TUI.HomeHidden = []string{"setup"}
	if err := Validate(doc); err == nil {
		t.Fatal("invented home id accepted")
	}
	if len(HomeIDs()) == 0 || len(ColumnIDs("machines")) == 0 || len(PreviewPanelIDs()) == 0 {
		t.Fatal("catalog API returned empty values")
	}
	copyIDs := HomeIDs()
	copyIDs[0] = "changed"
	if HomeIDs()[0] == "changed" {
		t.Fatal("catalog API exposed mutable storage")
	}
}

func TestFixedNavigationAliasesCannotBeAssignedElsewhere(t *testing.T) {
	for _, key := range []string{"up", "down", "ctrl+k", "ctrl+n"} {
		doc := Default()
		doc.TUI.Keys["select"] = key
		if err := Validate(doc); err == nil {
			t.Fatalf("accepted conflicting fixed key %s", key)
		}
	}
}
