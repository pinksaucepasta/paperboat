package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/spf13/cobra"
)

func preferenceTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "pb"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().Bool("no-customization", false, "")
	preview := &cobra.Command{Use: "preview [port]", Run: func(*cobra.Command, []string) {}}
	preview.Flags().Bool("background", false, "")
	tunnel := &cobra.Command{Use: "tunnel"}
	create := &cobra.Command{Use: "create [name]", Run: func(*cobra.Command, []string) {}}
	create.Flags().String("port", "", "")
	create.Flags().BoolP("wait", "w", false, "")
	tunnel.AddCommand(create)
	ssh := &cobra.Command{Use: "ssh machine", Aliases: []string{"connect-ssh"}, Run: func(*cobra.Command, []string) {}}
	for _, name := range []string{"scp", "sftp", "rsync"} {
		root.AddCommand(&cobra.Command{Use: name + " args", DisableFlagParsing: true, Run: func(*cobra.Command, []string) {}})
	}
	config := &cobra.Command{Use: "config"}
	config.AddCommand(&cobra.Command{Use: "customize", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(preview, tunnel, ssh, config)
	return root
}

func TestRawToolShortcutOptionAritiesAndGlobalFlags(t *testing.T) {
	root := preferenceTestRoot()
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{
		"upload":  {Command: []string{"scp"}, Args: []string{"{args}", "{1}", "mac:{2}"}},
		"sync":    {Command: []string{"rsync"}, Args: []string{"{args}", "{1}", "mac:{2}"}},
		"rawsftp": {Command: []string{"sftp"}, Args: []string{"{args}", "mac"}},
	}
	got, _, err := resolvePreferences(root, doc, []string{"upload", "-P", "2222", "local file", "remote path"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"scp", "-P", "2222", "local file", "mac:remote path"}) {
		t.Fatalf("scp expansion=%q", got)
	}
	got, _, err = resolvePreferences(root, doc, []string{"sync", "--exclude", "*.tmp", "-avz", "local/", "remote/"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"rsync", "--exclude", "*.tmp", "-avz", "local/", "mac:remote/"}) {
		t.Fatalf("rsync expansion=%q", got)
	}
	if _, _, err = resolvePreferences(root, doc, []string{"sync", "--unknown-option", "value", "local", "remote"}); err == nil {
		t.Fatal("ambiguous raw option accepted for indexed shortcut")
	}
	got, _, err = resolvePreferences(root, doc, []string{"rawsftp", "--vendor-option", "value"})
	if err != nil || !reflect.DeepEqual(got, []string{"sftp", "--vendor-option", "value", "mac"}) {
		t.Fatalf("raw passthrough=%q err=%v", got, err)
	}

	got, err = normalizeRawGlobalFlags(root, []string{"--server=before.example", "scp", "-P2222", "local", "mac:remote", "--json", "--config", "custom.json", "--", "--server", "external"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"scp", "-P2222", "local", "mac:remote", "--", "--server", "external"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw globals=%q want=%q", got, want)
	}
	if value, _ := root.PersistentFlags().GetString("config"); value != "custom.json" {
		t.Fatalf("config=%q", value)
	}
	if value, _ := root.PersistentFlags().GetString("server"); value != "before.example" {
		t.Fatalf("server=%q", value)
	}
	if value, _ := root.PersistentFlags().GetBool("json"); !value {
		t.Fatal("json flag was not applied")
	}
	if _, err := normalizeRawGlobalFlags(preferenceTestRoot(), []string{"scp", "local", "mac:path", "--config"}); err == nil {
		t.Fatal("missing global flag value accepted")
	}
}

func TestPreferenceShortcutExpansionIsArgvOnlyAndPreservesDash(t *testing.T) {
	root := preferenceTestRoot()
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{"mac": {Command: []string{"ssh"}, Args: []string{"mac", "{args}"}}, "copy": {Command: []string{"ssh"}, Args: []string{"{args}", "{2}@{1}"}}}
	if err := validatePreferencesCommands(root, doc); err != nil {
		t.Fatal(err)
	}
	got, _, err := resolvePreferences(root, doc, []string{"mac", ";rm -rf /", "--", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "mac", ";rm -rf /", "--", "--json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%q want=%q", got, want)
	}
	got, _, err = resolvePreferences(root, doc, []string{"copy", "host", "user"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"ssh", "user@host"}) {
		t.Fatalf("embedded template=%q", got)
	}
}

func TestPreferencePortActionAndCanonicalCollision(t *testing.T) {
	root := preferenceTestRoot()
	doc := preferences.Default()
	doc.PortAction = "tunnel"
	got, _, err := resolvePreferences(root, doc, []string{"3000"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"tunnel", "create", "--port", "3000"}) {
		t.Fatalf("port=%q", got)
	}
	doc.Shortcuts = map[string]preferences.Shortcut{"ssh": {Command: []string{"preview"}}}
	if err := validatePreferencesCommands(root, doc); err == nil {
		t.Fatal("canonical collision accepted")
	}
}

func TestPreferenceDefaultsRespectExplicitFlagsAndRejectSensitiveFlags(t *testing.T) {
	root := preferenceTestRoot()
	doc := preferences.Default()
	doc.Defaults = map[string]map[string]string{"tunnel create": {"wait": "true"}}
	if err := validatePreferencesCommands(root, doc); err != nil {
		t.Fatal(err)
	}
	got, explain, err := resolvePreferences(root, doc, []string{"tunnel", "create", "demo", "--wait=false"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"tunnel", "create", "demo", "--wait=false"}) || len(explain.Defaults) != 0 {
		t.Fatalf("explicit flag overwritten: %q %+v", got, explain)
	}
	got, _, err = resolvePreferences(root, doc, []string{"tunnel", "create", "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if got[len(got)-1] != "--wait=true" {
		t.Fatalf("default missing: %q", got)
	}
	doc.Defaults = map[string]map[string]string{"preview": {"json": "true"}}
	if err := validatePreferencesCommands(root, doc); err == nil || !strings.Contains(err.Error(), "cannot be configured") {
		t.Fatalf("sensitive default err=%v", err)
	}
	doc.Defaults = map[string]map[string]string{"tunnel create": {"wait": "not-bool"}}
	if err := validatePreferencesCommands(root, doc); err == nil {
		t.Fatal("invalid typed default accepted")
	}
}

func TestPreferenceRootFlagsAndOperandAccounting(t *testing.T) {
	root := preferenceTestRoot()
	doc := preferences.Default()
	doc.Shortcuts = map[string]preferences.Shortcut{"pick": {Command: []string{"ssh"}, Args: []string{"{args}", "{2}@{1}"}}}
	got, _, err := resolvePreferences(root, doc, []string{"--json", "pick", "host", "user"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"--json", "ssh", "user@host"}) {
		t.Fatalf("root flags/templates=%q", got)
	}
	doc.Shortcuts = map[string]preferences.Shortcut{"one": {Command: []string{"ssh"}, Args: []string{"{1}"}}}
	if _, _, err := resolvePreferences(root, doc, []string{"one"}); err == nil {
		t.Fatal("missing operand accepted")
	}
	if _, _, err := resolvePreferences(root, doc, []string{"one", "host", "extra"}); err == nil {
		t.Fatal("discarded operand accepted")
	}
}

func TestPreparePreferencesBypassDoesNotReadInvalidPreferences(t *testing.T) {
	root := preferenceTestRoot()
	for _, args := range [][]string{{"--config", "/no/such/parent/config.json", "--no-customization", "preview", "3000"}, {"--config", "/no/such/parent/config.json", "config", "customize"}} {
		got, ctx, err := preparePreferences(root, args, context.Background())
		if err != nil {
			t.Fatalf("args=%q err=%v", args, err)
		}
		if !reflect.DeepEqual(got, args) || preferences.FromContext(ctx).Version != 1 {
			t.Fatalf("bypass got=%q", got)
		}
	}
	for _, args := range [][]string{{"--config", "/no/such/parent/config.json", "--help"}, {"--config", "/no/such/parent/config.json", "help", "config"}, {"--config", "/no/such/parent/config.json", "--version"}} {
		if got, _, err := preparePreferences(root, args, context.Background()); err != nil || !reflect.DeepEqual(got, args) {
			t.Fatalf("recovery args=%q got=%q err=%v", args, got, err)
		}
	}
}

func TestPreferenceGlobalSelectionUsesLastOccurrenceBeforeDash(t *testing.T) {
	root := preferenceTestRoot()
	args := []string{"--config", "first.json", "--config=second.json", "--no-customization", "--no-customization=false", "preview", "3000", "--", "--config=remote.json", "--no-customization"}
	if got := preferenceFlagValue(root, args, "config"); got != "second.json" {
		t.Fatalf("config=%q", got)
	}
	if preferenceFlag(args, "no-customization") {
		t.Fatal("last pre-boundary false did not win")
	}
}
