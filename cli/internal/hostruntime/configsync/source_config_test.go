package configsync

import (
	"bytes"
	"context"
	"errors"
	"github.com/BurntSushi/toml"
	git "github.com/go-git/go-git/v5"
	"os"
	"path/filepath"
	"testing"
)

func stringPointer(value string) *string               { return &value }
func boolPointer(value bool) *bool                     { return &value }
func modePointer(value AssignmentMode) *AssignmentMode { return &value }
func patternsPointer(value ...string) *[]string        { return &value }
func completeRule(local, kind string) RuleConfig {
	return RuleConfig{LocalPath: stringPointer(local), Kind: stringPointer(kind)}
}
func TestSourceConfigStrictPresentAndOptionalMissing(t *testing.T) {
	for _, data := range []string{`{}`, `paths = null`, `version = 2`, "mode = 'pull_only'\nmode = 'push_only'", "[paths.a]\nkind = 'file'\nkind = 'directory'", `secret = 'password'`, "[paths.a]\ninclude = 1", `[os.plan9]`, `[os.linux.os]`, "[paths.a]\ninclude = ['[']", `enabled = 'true'`, `version = 1.0`, `paths = 3`, `os = 3`, `pull = 3`, `[Paths.a]`, "[paths.a]\nexclude = [1]"} {
		if _, err := ParseSourceConfig([]byte(data), DefaultSourceConfigLimits()); !errors.Is(err, ErrSourceConfigInvalid) {
			t.Fatalf("accepted invalid config %s: %v", data, err)
		}
	}
	path := filepath.Join(resolvedTempDir(t), "config-sync.toml")
	missing, err := LoadSourceConfig(path, DefaultSourceConfigLimits())
	if err != nil || missing.Paths != nil {
		t.Fatalf("missing %v %#v", err, missing)
	}
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSourceConfig(path, DefaultSourceConfigLimits()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`unexpected = true`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSourceConfig(path, DefaultSourceConfigLimits()); !errors.Is(err, ErrSourceConfigInvalid) {
		t.Fatalf("invalid present source fell back: %v", err)
	}
}
func TestSourceConfigPresenceInheritanceAndOSPrecedence(t *testing.T) {
	shared := SourceConfig{Mode: modePointer(ModePullOnly), AutomaticUpdates: boolPointer(true), Pull: &RepositoryTarget{RepositoryID: "shared"}, Paths: map[string]RuleConfig{"editor": {LocalPath: stringPointer("~/default"), Kind: stringPointer("directory"), Include: patternsPointer("*.json"), Exclude: patternsPointer("private/**")}}, OS: map[string]SourceConfig{"windows": {Paths: map[string]RuleConfig{"editor": {LocalPath: stringPointer("%APPDATA%/editor")}}}}}
	inherited, err := MergeSourceConfigsForOS(shared, SourceConfig{}, "windows")
	if err != nil {
		t.Fatal(err)
	}
	rule := inherited.PathRules[0]
	if inherited.Mode != ModePullOnly || !inherited.AutomaticUpdates || rule.Source != "os" || rule.LocalPath != "%APPDATA%/editor" || len(rule.Exclude) != 1 || inherited.Pull.RepositoryID != "shared" {
		t.Fatalf("shared/OS inheritance %#v", inherited)
	}
	machine := SourceConfig{AutomaticUpdates: boolPointer(false), Pull: &RepositoryTarget{RepositoryID: "machine"}, Paths: map[string]RuleConfig{"editor": {Exclude: patternsPointer(), Include: patternsPointer(), LocalPath: stringPointer("~/custom")}}}
	effective, err := MergeSourceConfigsForOS(shared, machine, "windows")
	if err != nil {
		t.Fatal(err)
	}
	rule = effective.PathRules[0]
	if effective.AutomaticUpdates || effective.Mode != ModePullOnly || rule.Source != "machine" || rule.Kind != "directory" || len(rule.Include)+len(rule.Exclude) != 0 || effective.Pull.RepositoryID != "machine" {
		t.Fatalf("presence override %#v", effective)
	}
	defaults, err := MergeSourceConfigsForOS(SourceConfig{}, SourceConfig{}, "linux")
	if err != nil || defaults.Mode != ModeBidirectional || defaults.AutomaticUpdates || !defaults.Enabled || len(defaults.PathRules) != 0 {
		t.Fatalf("empty defaults %#v %v", defaults, err)
	}
	if _, err := MergeSourceConfigsForOS(shared, SourceConfig{OS: map[string]SourceConfig{"linux": {}}}, "linux"); !errors.Is(err, ErrSourceConfigInvalid) {
		t.Fatal("machine OS section accepted")
	}
}
func TestSourceOwnershipWinsBeforeSelectorWithoutFallback(t *testing.T) {
	home := resolvedTempDir(t)
	sharedRoot := resolvedTempDir(t)
	osRoot := resolvedTempDir(t)
	machineRoot := resolvedTempDir(t)
	shared := SourceConfig{Paths: map[string]RuleConfig{"configs": completeRule(sharedRoot, "directory")}, OS: map[string]SourceConfig{"linux": {Paths: map[string]RuleConfig{"configs/editor": {LocalPath: stringPointer(osRoot), Kind: stringPointer("directory"), Exclude: patternsPointer("*.secret")}}}}}
	machine := SourceConfig{Paths: map[string]RuleConfig{"configs/editor/file.json": {LocalPath: stringPointer(filepath.Join(machineRoot, "renamed")), Kind: stringPointer("file")}}}
	effective, err := MergeSourceConfigsForOS(shared, machine, "linux")
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := ResolveEffectiveConfig(home, effective)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, want string
		ok         bool
	}{{"configs/a", filepath.Join(sharedRoot, "a"), true}, {"configs/editor/a.json", filepath.Join(osRoot, "a.json"), true}, {"configs/editor/a.secret", "", false}, {"configs/editor/file.json", filepath.Join(machineRoot, "renamed"), true}}
	for _, tc := range cases {
		got, ok := mapping.LocalPath(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ownership %s=>%s,%v", tc.name, got, ok)
		}
	}
	// A less-specific machine directory owns every descendant, including a
	// previously more-specific shared or OS file.
	machine = SourceConfig{Paths: map[string]RuleConfig{"configs": {LocalPath: stringPointer(machineRoot), Kind: stringPointer("directory"), Include: patternsPointer("*.txt")}}}
	effective, err = MergeSourceConfigsForOS(shared, machine, "linux")
	if err != nil {
		t.Fatal(err)
	}
	mapping, err = ResolveEffectiveConfig(home, effective)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mapping.LocalPath("configs/editor/a.json"); ok {
		t.Fatal("losing OS owner bypassed machine selector")
	}
}
func TestSourceConfigChangesPauseBeforeMappedLocalIO(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = []PathRule{{ID: "machine:config", Source: "machine", RepositoryPath: "config", LocalPath: filepath.Join(home, "config"), Kind: "file"}}
	source := SourceConfig{Mode: modePointer(descriptor.Mode), Paths: map[string]RuleConfig{"config": completeRule(filepath.Join(home, "config"), "file")}}
	writeTestFile(t, filepath.Join(repoRoot, "config"), "remote")
	repository := initSourceTestRepository(t, repoRoot)
	reconciler, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor, MachineSource: source})
	if err != nil {
		t.Fatal(err)
	}
	// Existing approved destination becomes a symlink. Source mismatch must
	// win before even resolving that destination or probing its target.
	link := filepath.Join(home, "config")
	if err := os.Symlink(filepath.Join(home, "unknown"), link); err != nil {
		t.Skip(err)
	}
	if err := os.Mkdir(filepath.Join(repoRoot, ".paperboat"), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := marshalTestSource(SourceConfig{AutomaticUpdates: boolPointer(true)})
	if err := os.WriteFile(filepath.Join(repoRoot, ".paperboat/config-sync.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	head := commitAll(t, repository, "new shared defaults")
	if _, err := reconciler.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head}); !errors.Is(err, ErrConfigurationChanged) {
		t.Fatalf("mapped IO occurred before source approval: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "unknown")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unapproved source touched mapped destination")
	}
}

func TestSourceNestedOverrideAtCorrespondingDestination(t *testing.T) {
	home, state, repoRoot := resolvedTempDir(t), resolvedTempDir(t), resolvedTempDir(t)
	destination := filepath.Join(home, "foo")
	shared := SourceConfig{Mode: modePointer(ModePullOnly), Paths: map[string]RuleConfig{"foo": completeRule(destination, "directory")}}
	machine := SourceConfig{Paths: map[string]RuleConfig{"foo/settings.json": completeRule(filepath.Join(destination, "settings.json"), "file")}}
	effective, err := MergeSourceConfigs(shared, machine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveEffectiveConfig(home, effective); err != nil {
		t.Fatalf("corresponding nested override rejected: %v", err)
	}
	for _, invalid := range []string{filepath.Join(destination, "other.json"), destination} {
		machine.Paths["foo/settings.json"] = completeRule(invalid, "file")
		bad, err := MergeSourceConfigs(shared, machine)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveEffectiveConfig(home, bad); !errors.Is(err, ErrPathRuleInvalid) {
			t.Fatalf("ambiguous destination accepted %s: %v", invalid, err)
		}
	}
	machine.Paths["foo/settings.json"] = completeRule(filepath.Join(destination, "settings.json"), "file")
	for _, dir := range []string{".paperboat", "foo"} {
		if err := os.Mkdir(filepath.Join(repoRoot, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := marshalTestSource(shared)
	writeTestFile(t, filepath.Join(repoRoot, SharedSourceConfigPath), string(data))
	writeTestFile(t, filepath.Join(repoRoot, "foo/settings.json"), "overridden settings")
	writeTestFile(t, filepath.Join(repoRoot, "foo/other.json"), "inherited settings")
	repo := initSourceTestRepository(t, repoRoot)
	head := commitAll(t, repo, "nested shared and machine ownership")
	descriptor := testRuntimeDescriptor()
	descriptor.Mode, descriptor.PathRules, descriptor.ConfigurationRevision = effective.Mode, effective.PathRules, effective.Revision
	r, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor, MachineSource: machine})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"settings.json": "overridden settings", "other.json": "inherited settings"} {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(got) != want {
			t.Fatalf("nested ownership %s=%q, %v", name, got, err)
		}
	}
}

func initSourceTestRepository(t *testing.T, root string) *git.Repository {
	t.Helper()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSharedFileDefaultsWithEmptyMachineFileReconcileWithoutCopyingDefaults(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	destination := filepath.Join(resolvedTempDir(t), "settings")
	shared := SourceConfig{Mode: modePointer(ModePullOnly), AutomaticUpdates: boolPointer(true), Paths: map[string]RuleConfig{"editor/settings.json": completeRule(destination, "file")}}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".paperboat"), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := marshalTestSource(shared)
	if err := os.WriteFile(filepath.Join(repoRoot, ".paperboat/config-sync.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repoRoot, "editor"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repoRoot, "editor/settings.json"), "shared settings")
	machinePath := filepath.Join(resolvedTempDir(t), "config-sync.toml")
	writeTestFile(t, machinePath, "")
	machine, err := LoadSourceConfig(machinePath, DefaultSourceConfigLimits())
	if err != nil {
		t.Fatal(err)
	}
	effective, err := MergeSourceConfigs(shared, machine)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testRuntimeDescriptor()
	descriptor.Mode = effective.Mode
	descriptor.AutomaticUpdates = effective.AutomaticUpdates
	descriptor.PathRules = effective.PathRules
	descriptor.ConfigurationRevision = effective.Revision
	repository := initSourceTestRepository(t, repoRoot)
	head := commitAll(t, repository, "approved shared")
	r, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor, MachineSource: machine})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PublicationCommitted(context.Background(), prepared, prepared.CommitID); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(destination)
	if string(got) != "shared settings" {
		t.Fatalf("shared default not applied: %q", got)
	}
	got, _ = os.ReadFile(machinePath)
	if string(got) != "" {
		t.Fatalf("shared defaults copied into machine file: %q", got)
	}
	baseline, err := ReadBaseline(filepath.Join(state, "baseline.json"))
	if err != nil || len(baseline.Files) != 1 {
		t.Fatalf("metadata treated as payload: %#v %v", baseline, err)
	}
}
func TestDirectionalChildUsesParentSourceApprovalAndPrimarySharedCallback(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	source := SourceConfig{Mode: modePointer(ModeBidirectional), Paths: map[string]RuleConfig{"config": completeRule(filepath.Join(home, "config"), "file")}}
	effective, err := MergeSourceConfigs(source, SourceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	parent := testRuntimeDescriptor()
	parent.ConfigurationRevision = effective.Revision
	parent.PathRules = effective.PathRules
	child := parent
	child.Mode = ModePushOnly
	writeTestFile(t, filepath.Join(repoRoot, "config"), "same")
	writeTestFile(t, filepath.Join(home, "config"), "same")
	repo := initSourceTestRepository(t, repoRoot)
	head := commitAll(t, repo, "source lives in pull repository")
	calls := 0
	r, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: child, ApprovedConfiguration: &parent, SharedSource: func(context.Context) (SourceConfig, error) { calls++; return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head}); err != nil {
		t.Fatalf("directional IO incorrectly changed source projection: %v", err)
	}
	if calls != 1 {
		t.Fatalf("primary shared callback count %d", calls)
	}
}
func TestPresentInvalidSharedSourceCannotFallBackToApprovedMachineRules(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	source := SourceConfig{Paths: map[string]RuleConfig{"config": completeRule(filepath.Join(home, "config"), "file")}}
	effective, err := MergeSourceConfigs(SourceConfig{}, source)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = effective.PathRules
	descriptor.ConfigurationRevision = effective.Revision
	if err := os.Mkdir(filepath.Join(repoRoot, ".paperboat"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repoRoot, ".paperboat/config-sync.toml"), `paths = 3`)
	writeTestFile(t, filepath.Join(repoRoot, "config"), "remote")
	repo := initSourceTestRepository(t, repoRoot)
	head := commitAll(t, repo, "invalid shared present")
	r, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor, MachineSource: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head}); !errors.Is(err, ErrSourceConfigInvalid) {
		t.Fatalf("invalid shared fell back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid shared source applied local file")
	}
}

func TestNestedSourceOwnersReconcileRealFilesWithoutReadingLosingPayload(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	sharedRoot := resolvedTempDir(t)
	machineRoot := resolvedTempDir(t)
	for _, directory := range []string{filepath.Join(repoRoot, ".paperboat"), filepath.Join(repoRoot, "configs/sub"), filepath.Join(sharedRoot, "sub")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(repoRoot, "configs/general"), "general")
	writeTestFile(t, filepath.Join(sharedRoot, "general"), "general")
	writeTestFile(t, filepath.Join(repoRoot, "configs/sub/settings.json"), "winning machine")
	writeTestFile(t, filepath.Join(machineRoot, "settings.json"), "winning machine")
	writeTestFile(t, filepath.Join(sharedRoot, "sub/settings.json"), "losing local payload")
	writeTestFile(t, filepath.Join(repoRoot, "configs/sub/private.secret"), "retained upstream")
	writeTestFile(t, filepath.Join(sharedRoot, "sub/private.secret"), "retained local")
	shared := SourceConfig{Paths: map[string]RuleConfig{"configs": completeRule(sharedRoot, "directory")}}
	data, _ := marshalTestSource(shared)
	if err := os.WriteFile(filepath.Join(repoRoot, ".paperboat/config-sync.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	machine := SourceConfig{Paths: map[string]RuleConfig{"configs/sub": {LocalPath: stringPointer(machineRoot), Kind: stringPointer("directory"), Exclude: patternsPointer("*.secret")}}}
	data, _ = marshalTestSource(machine)
	machinePath := filepath.Join(resolvedTempDir(t), "config-sync.toml")
	if err := os.WriteFile(machinePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	machine, err := LoadSourceConfig(machinePath, DefaultSourceConfigLimits())
	if err != nil {
		t.Fatal(err)
	}
	effective, err := MergeSourceConfigs(shared, machine)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = effective.PathRules
	descriptor.ConfigurationRevision = effective.Revision
	repo := initSourceTestRepository(t, repoRoot)
	head := commitAll(t, repo, "explicit nested ownership")
	r, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor, MachineSource: machine})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.HasChanges || len(r.Diagnostics().Conflicts) != 0 {
		t.Fatalf("losing owner participated: %#v %#v", prepared, r.Diagnostics())
	}
	for path, want := range map[string]string{filepath.Join(sharedRoot, "sub/settings.json"): "losing local payload", filepath.Join(sharedRoot, "sub/private.secret"): "retained local", filepath.Join(repoRoot, "configs/sub/private.secret"): "retained upstream"} {
		got, _ := os.ReadFile(path)
		if string(got) != want {
			t.Fatalf("unselected path mutated: %q", got)
		}
	}
	if _, err := os.Stat(filepath.Join(machineRoot, "private.secret")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("losing shared rule bypassed machine exclusion")
	}
}

func marshalTestSource(source SourceConfig) ([]byte, error) {
	var buf bytes.Buffer
	err := toml.NewEncoder(&buf).Encode(source)
	return buf.Bytes(), err
}

func TestSourceTOMLCommentsQuotedPathsAndLiteralWindowsStrings(t *testing.T) {
	source, err := ParseSourceConfig([]byte("# empty defaults are valid\n"), DefaultSourceConfigLimits())
	if err != nil || source.Paths != nil {
		t.Fatalf("comment-only source: %#v %v", source, err)
	}
	data := `version = 1
# A literal string preserves the user's Windows destination.
[os.windows.paths."editor/settings.json"]
local_path = 'C:\Users\Sailor\AppData\Roaming\editor\settings.json'
kind = 'file'
[paths."shell/config"]
local_path = '~/shell/config'
kind = 'file'
exclude = []
`
	source, err = ParseSourceConfig([]byte(data), DefaultSourceConfigLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := *source.OS["windows"].Paths["editor/settings.json"].LocalPath; got != `C:\Users\Sailor\AppData\Roaming\editor\settings.json` {
		t.Fatalf("literal destination %q", got)
	}
	if rule := source.Paths["shell/config"]; rule.Include != nil || rule.Exclude == nil || len(*rule.Exclude) != 0 {
		t.Fatalf("array presence lost: %#v", rule)
	}
}
