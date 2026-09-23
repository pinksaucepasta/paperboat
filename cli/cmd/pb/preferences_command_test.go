package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/spf13/cobra"
)

func TestCustomizationImportShowAndInvalidRecovery(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	path, _ := preferences.Path(configPath)
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{"where": {Command: []string{"config", "path"}, Args: []string{"{args}"}}}
	doc.TUI.Theme = "mono"
	source := filepath.Join(directory, "incoming.json")
	if err := preferences.Save(source, doc); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (int, string, string) {
		var out, err bytes.Buffer
		code := run(context.Background(), append([]string{"--config", configPath}, args...), &out, &err)
		return code, out.String(), err.String()
	}
	code, out, errOut := invoke("config", "customize", "import", source, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("import: %d %s %s", code, out, errOut)
	}
	code, out, errOut = invoke("config", "customize", "show", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("show: %d %s %s", code, out, errOut)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Scope       string               `json:"scope"`
			Preferences preferences.Document `json:"preferences"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || !envelope.OK || envelope.Data.Scope != "local" || envelope.Data.Preferences.TUI.Theme != "mono" {
		t.Fatalf("show: %s (%v)", out, err)
	}
	code, out, errOut = invoke("where", "--json")
	if code != 0 || errOut != "" || !strings.Contains(out, configPath) {
		t.Fatalf("shortcut dispatch: %d %s %s", code, out, errOut)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"bogus":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = invoke("config", "path", "--json")
	if code != 2 || errOut != "" || !strings.Contains(out, "no-customization") {
		t.Fatalf("invalid config: %d %s %s", code, out, errOut)
	}
	code, out, errOut = invoke("--no-customization", "config", "path", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("bypass: %d %s %s", code, out, errOut)
	}
	code, out, errOut = invoke("config", "customize", "reset", "--yes", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("reset recovery: %d %s %s", code, out, errOut)
	}
	repaired, err := preferences.Load(path)
	if err != nil || len(repaired.Shortcuts) != 0 {
		t.Fatalf("reset did not recover: %+v %v", repaired, err)
	}
}

func TestCustomizationInvalidImportPreservesSavedFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	path, _ := preferences.Path(configPath)
	doc := preferences.Default()
	doc.TUI.Theme = "light"
	if err := preferences.Save(path, doc); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(source, []byte(`{"version":1,"shortcuts":{"ssh":{"command":["connect"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", configPath, "config", "customize", "import", source, "--json"}, &out, &stderr)
	if code == 0 || stderr.Len() != 0 {
		t.Fatalf("invalid import = %d %s %s", code, &out, &stderr)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid import changed saved preferences")
	}
}

func TestPersonalizedHomeOrderVisibilityAndPinnedActions(t *testing.T) {
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{"mac": {Command: []string{"ssh"}, Args: []string{"mac", "{args}"}}}
	doc.TUI.Favorites = []string{"mac"}
	doc.TUI.HomeOrder = []string{"previews", "machines"}
	doc.TUI.HomeHidden = []string{"team", "commands", "customize"}
	items := personalizedHomeItems(doc)
	if items[0].ID != "shortcut:mac" || items[1].ID != "previews" || items[2].ID != "machines" {
		t.Fatalf("wrong order: %+v", items)
	}
	ids := []string{}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if slices.Contains(ids, "team") || !slices.Contains(ids, "commands") || !slices.Contains(ids, "customize") {
		t.Fatalf("visibility/recovery: %v", ids)
	}
}

func TestPreferenceColumnsPreserveExplicitEmptyAndOrder(t *testing.T) {
	doc := preferences.Default()
	doc.TUI.Columns["machines"] = []string{"id", "status"}
	ctx := preferences.WithContext(context.Background(), doc)
	if got := preferenceDetails(ctx, "machines", map[string]string{"id": "m1", "status": "online", "platform": "linux"}); got != "m1 · online" {
		t.Fatal(got)
	}
	doc.TUI.Columns["machines"] = []string{}
	ctx = preferences.WithContext(context.Background(), doc)
	if got := preferenceDetails(ctx, "machines", map[string]string{"status": "online"}); got != "" {
		t.Fatal(got)
	}
}

func TestCustomizationExplainNeverExecutes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	path, _ := preferences.Path(configPath)
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{"mac": {Command: []string{"ssh"}, Args: []string{"mac", "{args}"}}}
	if err := preferences.Save(path, doc); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", configPath, "config", "customize", "explain", "--json", "--", "mac", "--", "echo", "a;$(touch nowhere)"}, &out, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("explain: %d %s %s", code, &out, &stderr)
	}
	var result struct {
		Data struct {
			Argv     []string `json:"argv"`
			Executes bool     `json:"executes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Executes || !slices.Contains(result.Data.Argv, "a;$(touch nowhere)") || !slices.Contains(result.Data.Argv, "ssh") {
		t.Fatalf("explain altered args: %s", &out)
	}
}

func TestNestedCustomizationBypassIsBoolean(t *testing.T) {
	root := newRootCommand()
	if err := root.PersistentFlags().Set("no-customization", "true"); err != nil {
		t.Fatal(err)
	}
	got := interactiveArgs(root, []string{"config", "path"})
	if !slices.Equal(got, []string{"--no-customization", "config", "path"}) {
		t.Fatalf("args: %q", got)
	}
}

func TestPreferenceConflictJSONIsActionable(t *testing.T) {
	for _, err := range []error{preferences.ErrChanged, preferences.ErrBusy} {
		got := classifyCLIJSONError(err)
		if got.Category != "conflict" || got.StateChanged != false || got.Recovery == "" {
			t.Fatalf("conflict contract: %+v", got)
		}
	}
}

func TestRawToolCustomizationUsesActualCobraBoundary(t *testing.T) {
	for _, name := range []string{"scp", "sftp", "rsync"} {
		t.Run(name, func(t *testing.T) {
			root := newRootCommand()
			target, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			called := false
			target.RunE = func(c *cobra.Command, args []string) error {
				called = true
				if got := configPathFlag(c); got != path {
					t.Errorf("config=%q want=%q", got, path)
				}
				if !slices.Equal(args, []string{"source", "mac:path"}) {
					t.Errorf("tool argv=%q", args)
				}
				return nil
			}
			args, ctx, err := preparePreferences(root, []string{"--no-customization", name, "--config", path, "source", "mac:path"}, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			root.SetArgs(args)
			if err = root.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("raw command never reached")
			}
			root = newRootCommand()
			target, _, _ = root.Find([]string{name})
			called = false
			target.RunE = func(*cobra.Command, []string) error { called = true; return nil }
			args, ctx, err = preparePreferences(root, []string{"--no-customization", name, "--json", "source", "mac:path"}, context.Background())
			if err != nil {
				t.Fatal(err)
			}
			root.SetArgs(args)
			err = root.ExecuteContext(ctx)
			var unsupported unsupportedJSONOutputError
			if called || !errors.As(err, &unsupported) {
				t.Fatalf("JSON reached raw execution: called=%v err=%v", called, err)
			}
		})
	}
}

func TestCustomizationCompletionIncludesLocalShortcuts(t *testing.T) {
	root := newRootCommand()
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{"macbook": {Command: []string{"ssh"}, Args: []string{"macbook", "{args}"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root.SetContext(preferences.WithContext(ctx, doc))
	values, _ := root.ValidArgsFunction(root, nil, "mac")
	if !slices.Contains(values, "macbook\tLocal shortcut for pb ssh") {
		t.Fatalf("completion omitted shortcut: %q", values)
	}
	root.SetContext(preferences.WithContext(ctx, preferences.Default()))
	values, _ = root.ValidArgsFunction(root, nil, "mac")
	if slices.Contains(values, "macbook\tLocal shortcut for pb ssh") {
		t.Fatal("shortcut leaked between contexts")
	}
}

func TestPreferenceArgumentEditorFormattingRoundTripsLiteralText(t *testing.T) {
	args := []string{"", "a b", "a'b", "$(touch never)", "a;b", "C:\\path\\file", "mac:{2}", "{args}"}
	parsed, err := splitPreferenceArgs(formatPreferenceArgs(args))
	if err != nil || !slices.Equal(args, parsed) {
		t.Fatalf("roundtrip=%q err=%v", parsed, err)
	}
}

func TestRawToolOptionValueIsNotAPaperboatFlag(t *testing.T) {
	root := newRootCommand()
	target, _, _ := root.Find([]string{"rsync"})
	called := false
	target.RunE = func(c *cobra.Command, args []string) error {
		called = true
		if !slices.Equal(args, []string{"--exclude", "--json", "", "mac:path"}) {
			t.Fatalf("raw operands changed: %q", args)
		}
		return nil
	}
	args, ctx, err := preparePreferences(root, []string{"--no-customization", "rsync", "--exclude", "--json", "", "mac:path"}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root.SetArgs(args)
	if err = root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("option value incorrectly enabled JSON")
	}
}

func TestPreferenceArgumentParserRejectsIncompleteInputAndNeverExpands(t *testing.T) {
	for _, input := range []string{"'open", "\"open", "trailing\\"} {
		if _, err := splitPreferenceArgs(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	got, err := splitPreferenceArgs(`echo '' "$HOME" '*.txt' ';' '$(whoami)'`)
	want := []string{"echo", "", "$HOME", "*.txt", ";", "$(whoami)"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("args=%q err=%v", got, err)
	}
}
