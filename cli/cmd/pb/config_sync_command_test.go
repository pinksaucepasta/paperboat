package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/adrg/xdg"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/spf13/cobra"
)

type configSyncSourceFixture struct {
	repos  []api.ConfigRepository
	grants map[string]configsync.RepositoryReadAccess
	calls  []string
}

func (f *configSyncSourceFixture) ListConfigRepositories(context.Context) ([]api.ConfigRepository, error) {
	return f.repos, nil
}
func (f *configSyncSourceFixture) ConfigRepositoryReadAccess(_ context.Context, id, url string) (configsync.RepositoryReadAccess, error) {
	f.calls = append(f.calls, id)
	a, ok := f.grants[id]
	if !ok {
		return a, errors.New("not registered")
	}
	if url != "" && a.CloneURL != url {
		return a, errors.New("endpoint unauthorized")
	}
	return a, nil
}
func configSyncSourceBare(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(shorttemp.Dir(t), "remote.git")
	repo, err := git.PlainInit(path, true)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value interface {
		Encode(plumbing.EncodedObject) error
	}) plumbing.Hash {
		encoded := repo.Storer.NewEncodedObject()
		if err := value.Encode(encoded); err != nil {
			t.Fatal(err)
		}
		hash, err := repo.Storer.SetEncodedObject(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	tree := &object.Tree{}
	if source != "" {
		blob := repo.Storer.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		writer, _ := blob.Writer()
		writer.Write([]byte(source))
		writer.Close()
		hash, err := repo.Storer.SetEncodedObject(blob)
		if err != nil {
			t.Fatal(err)
		}
		nested := encode(&object.Tree{Entries: []object.TreeEntry{{Name: filepath.Base(configsync.SharedSourceConfigPath), Mode: filemode.Regular, Hash: hash}}})
		tree.Entries = []object.TreeEntry{{Name: ".paperboat", Mode: filemode.Dir, Hash: nested}}
	}
	treeHash := encode(tree)
	signature := object.Signature{Name: "test", Email: "test@invalid", When: time.Now()}
	hash := encode(&object.Commit{Author: signature, Committer: signature, Message: "source", TreeHash: treeHash})
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), hash)); err != nil {
		t.Fatal(err)
	}
	repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")))
	return path
}
func configSyncSourceBackendFixture(t *testing.T, sources map[string]string) *configSyncSourceFixture {
	t.Helper()
	f := &configSyncSourceFixture{grants: map[string]configsync.RepositoryReadAccess{}}
	for id, source := range sources {
		url := configSyncSourceBare(t, source)
		f.repos = append(f.repos, api.ConfigRepository{ID: id, DisplayName: id, ExternalRef: url, Provider: "git"})
		f.grants[id] = configsync.RepositoryReadAccess{RepositoryID: id, CloneURL: url, Branch: "main", Transport: "local", Capability: "repository_contents_read", ExpiresAt: time.Now().Add(5 * time.Minute)}
	}
	return f
}
func TestConfigSyncSourceInitNoOverwriteOrLinkedParent(t *testing.T) {
	root := shorttemp.Dir(t)
	path := filepath.Join(root, "new", "config-sync.toml")
	if err := initMachineConfigSource(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "# Paperboat config sync\n" {
		t.Fatal("unexpected initialized source")
	}
	if err := initMachineConfigSource(path); err == nil || !errors.Is(err, os.ErrExist) || !errors.Is(err, errUsage) {
		t.Fatalf("existing source refusal lost its expected classification: %v", err)
	} else if strings.Contains(err.Error(), path) {
		t.Fatalf("existing source path leaked in refusal: %q", err.Error())
	}
	other := shorttemp.Dir(t)
	link := filepath.Join(root, "link")
	if err := os.Symlink(other, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	recoveryPath := filepath.Join(link, "source.json")
	if err := initMachineConfigSource(recoveryPath); err == nil || strings.Contains(err.Error(), link) {
		t.Fatalf("linked parent accepted or leaked its path: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(link, 0700); err != nil {
		t.Fatal(err)
	}
	if err := initMachineConfigSource(recoveryPath); err != nil {
		t.Fatalf("source initialization did not recover after repairing its parent: %v", err)
	}
	if data, err := os.ReadFile(recoveryPath); err != nil || string(data) != "# Paperboat config sync\n" {
		t.Fatalf("recovered source was not initialized: %q %v", data, err)
	}
}
func TestConfigSyncSourceProjectionActualFilesOnlyAndMachineOverride(t *testing.T) {
	t.Setenv("PATH", "")
	backend := configSyncSourceBackendFixture(t, map[string]string{"one": `"version" = 1
"automatic_updates" = true
["paths"]
["paths"."editor"]
"local_path" = "~/editor.json"
"kind" = "file"
`})
	machine, err := configsync.ParseSourceConfig([]byte(`["paths"]
["paths"."editor"]
"local_path" = "~/local.json"
`), configsync.DefaultSourceConfigLimits())
	if err != nil {
		t.Fatal(err)
	}
	home := shorttemp.Dir(t)
	projection, err := buildConfigSyncProjection(context.Background(), backend, machine, "one", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", home)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := configsync.ParseSourceConfig([]byte(`"version" = 1
"automatic_updates" = true
["paths"]
["paths"."editor"]
"local_path" = "~/editor.json"
"kind" = "file"
`), configsync.DefaultSourceConfigLimits())
	effective, _ := configsync.MergeSourceConfigs(shared, machine, "")
	if projection.Revision != effective.Revision || projection.PullRepositoryID != "one" || projection.PushRepositoryID != "one" || len(projection.PathRules) != 1 || projection.PathRules[0].LocalPath != "~/local.json" || !projection.AutomaticUpdates {
		t.Fatalf("bad projection %+v", projection)
	}
	if projection.SharedRevision == "" {
		t.Fatal("observed commit omitted")
	}
}
func TestConfigSyncSourceRedirectPrimaryBoundedAndBranchStrict(t *testing.T) {
	t.Setenv("PATH", "")
	backend := configSyncSourceBackendFixture(t, map[string]string{"one": `["pull"]
"repository_id" = "two"
`, "two": `["pull"]
"repository_id" = "two"
`})
	projection, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, "one", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
	if err != nil || projection.PullRepositoryID != "two" {
		t.Fatal("author-selected primary not followed", err)
	}
	backend = configSyncSourceBackendFixture(t, map[string]string{"one": `["pull"]
"repository_id" = "two"
`, "two": `["pull"]
"repository_id" = "one"
`})
	if _, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, "one", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t)); err == nil || !strings.Contains(err.Error(), "repeatedly") {
		t.Fatal("primary loop accepted", err)
	}
	backend = configSyncSourceBackendFixture(t, map[string]string{"one": ""})
	machine, _ := configsync.ParseSourceConfig([]byte(`["pull"]
"repository_id" = "one"
"branch" = "other"
`), configsync.DefaultSourceConfigLimits())
	if _, err := buildConfigSyncProjection(context.Background(), backend, machine, "", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t)); err == nil {
		t.Fatal("unauthorized branch accepted")
	}
}
func TestConfigSyncSourceNoOpDisabledAndConfirmationFencing(t *testing.T) {
	t.Setenv("PATH", "")
	backend := configSyncSourceBackendFixture(t, map[string]string{"one": "", "two": ""})
	one, two := "one", "two"
	current := api.ConfigAssignment{PullRepositoryID: &one, PushRepositoryID: &two}
	projection, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, "", current, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
	if err != nil || len(projection.PathRules) != 0 || projection.PullRepositoryID != "one" || projection.PushRepositoryID != "two" {
		t.Fatal("empty file projection/current distinct target", err)
	}
	scope := configSyncProjectionConfirmationScope("machine", 2, projection)
	for _, mutation := range []func(*configSyncProjection){func(p *configSyncProjection) { p.SharedRevision = "other" }, func(p *configSyncProjection) { p.Revision = "other" }, func(p *configSyncProjection) { p.PushRepositoryID = "other" }} {
		copy := projection
		mutation(&copy)
		if scope == configSyncProjectionConfirmationScope("machine", 2, copy) {
			t.Fatal("stale projection scope accepted")
		}
	}
	if scope == configSyncProjectionConfirmationScope("other-machine", 2, projection) || scope == configSyncProjectionConfirmationScope("machine", 3, projection) {
		t.Fatal("stale machine/version scope accepted")
	}
	enabled := false
	disabled, err := buildConfigSyncProjection(context.Background(), &configSyncSourceFixture{}, configsync.SourceConfig{Enabled: &enabled}, "", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
	if err != nil || disabled.Enabled {
		t.Fatal("disable requires repository", err)
	}
}

func TestConfigSyncSourceCommandPreviewConsentAndStaleConfirmation(t *testing.T) {
	root := shorttemp.Dir(t)
	state := filepath.Join(root, "state")
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", state)
	t.Setenv("HOME", root)
	previous := xdg.ConfigHome
	xdg.ConfigHome = filepath.Join(root, "xdg")
	t.Cleanup(func() { xdg.ConfigHome = previous })
	bare := configSyncSourceBare(t, `"version" = 1
["paths"]
["paths"."editor"]
"local_path" = "~/editor.json"
"kind" = "file"
`)
	mutations, consents, started := 0, 0, 0
	version := int64(0)
	var submitted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("account read missing authentication")
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/config-repositories":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "repo", "display_name": "Source", "provider": "git", "external_ref": bare}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case "POST /v1/config-repositories/repo/read-access":
			writeAPIData(t, w, configsync.RepositoryReadAccess{RepositoryID: "repo", CloneURL: bare, Branch: "main", Transport: "local", Capability: "repository_contents_read", ExpiresAt: time.Now().Add(5 * time.Minute)})
		case "GET /v1/machines/machine/config-assignment":
			writeAPIData(t, w, map[string]any{"version": version})
		case "PUT /v1/machines/machine/config-assignment":
			mutations++
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Error(err)
			}
			if submitted["expected_version"] != float64(version) {
				t.Error("wrong optimistic version")
			}
			writeAPIData(t, w, map[string]any{"id": "assignment", "version": version + 1, "consent_state": "pending"})
		case "GET /v1/machines":
			writeAPIData(t, w, api.UserMachinePage{Items: []api.UserMachine{{ID: "machine", Alias: "machine", Platform: "linux"}}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case "GET /v1/machines/machine/config-assignment/warning":
			writeAPIData(t, w, map[string]any{"revision": "warning-exact", "repository_visibility": "ordinary plaintext", "history_retention": "Git history retains old values", "access_consequence": "collaborators can read content"})
		case "POST /v1/machines/machine/config-assignment/consent":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["warning_revision"] != "warning-exact" || body["expected_version"] != float64(version+1) {
				t.Error("wrong consent fencing")
			}
			consents++
			writeAPIData(t, w, map[string]any{"id": "assignment", "version": version + 2, "consent_state": "accepted"})
		default:
			t.Errorf("unexpected endpoint %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configPath := filepath.Join(root, "config.json")
	writeTestProfile(t, root, configPath, srv.URL)
	store, err := identity.Open(identity.Config{StateRoot: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRegistration(identity.Registration{ServerURL: srv.URL, MachineID: "machine", EnvironmentID: "environment", PublicKeyID: store.Current().ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(store.Current().Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	path, err := configsync.DefaultMachineSourcePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := initMachineConfigSource(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	execute := func(operation string, extra ...string) (error, string) {
		command := newRootCommand()
		command.SetContext(context.Background())
		leaf, _, err := command.Find([]string{"config", "sync", operation})
		if err != nil {
			t.Fatal(err)
		}
		if operation == "apply" {
			leaf.RunE = func(cmd *cobra.Command, args []string) error {
				return runConfigSyncSourceCommandWithService(cmd, args, true, func(_ context.Context, id string, install bool) error {
					if id != "machine" || !install {
						t.Error("wrong local service action")
					}
					started++
					return nil
				})
			}
		}
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		args := append([]string{"--config", configPath, "config", "sync", operation, "--repository", "repo"}, extra...)
		command.SetArgs(args)
		err = command.Execute()
		return err, output.String()
	}
	err, output := execute("apply")
	if err == nil || mutations != 0 || !strings.Contains(output, "ordinary plaintext") {
		t.Fatalf("preview mutated or omitted consent: %v %s", err, output)
	}
	token := previewConfirmationCode(t, output)
	version = 1
	if err, _ := execute("apply", "--confirm", token); err == nil || mutations != 0 {
		t.Fatal("stale assignment confirmation accepted")
	}
	version = 0
	err, output = execute("apply")
	if err == nil {
		t.Fatal("preview skipped")
	}
	token = previewConfirmationCode(t, output)
	machine := []byte(`"automatic_updates" = true
`)
	os.WriteFile(path, machine, 0600)
	if err, _ := execute("apply", "--confirm", token); err == nil || mutations != 0 {
		t.Fatal("changed file confirmed silently")
	}
	os.WriteFile(path, []byte("# Paperboat config sync\n"), 0600)
	err, output = execute("apply")
	if err == nil {
		t.Fatal("preview skipped")
	}
	token = previewConfirmationCode(t, output)
	repo, openErr := git.PlainOpen(bare)
	if openErr != nil {
		t.Fatal(openErr)
	}
	head, _ := repo.Head()
	commit, _ := repo.CommitObject(head.Hash())
	commit.ParentHashes = []plumbing.Hash{head.Hash()}
	commit.Message = "new commit, same source"
	encoded := repo.Storer.NewEncodedObject()
	commit.Encode(encoded)
	hash, _ := repo.Storer.SetEncodedObject(encoded)
	repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), hash))
	if err, _ := execute("apply", "--confirm", token); err == nil || mutations != 0 {
		t.Fatal("changed observed Git commit confirmed silently")
	}
	err, output = execute("apply")
	if err == nil {
		t.Fatal("preview skipped")
	}
	token = previewConfirmationCode(t, output)
	if err, output = execute("apply", "--confirm", token, "--json"); err != nil {
		t.Fatalf("apply failed %v %s", err, output)
	}
	if mutations != 1 || consents != 1 || started != 1 {
		t.Fatalf("missing configure/consent/service: %d %d %d", mutations, consents, started)
	}
	shared, _ := configsync.ParseSourceConfig([]byte(`"version" = 1
["paths"]
["paths"."editor"]
"local_path" = "~/editor.json"
"kind" = "file"
`), configsync.DefaultSourceConfigLimits())
	effective, _ := configsync.MergeSourceConfigs(shared, configsync.SourceConfig{}, "")
	if submitted["configuration_revision"] != effective.Revision {
		t.Fatal("API configuration revision differs from actual files")
	}
	rules, ok := submitted["path_rules"].([]any)
	if !ok || len(rules) != 1 || rules[0].(map[string]any)["source"] != "shared" {
		t.Fatal("rule provenance omitted")
	}
	if err, _ := execute("apply", "--confirm", token); err == nil || mutations != 1 {
		t.Fatal("confirmation replay accepted")
	}
	err, output = execute("validate", "--json")
	if err != nil || !strings.Contains(output, `"schema_version":"1.0"`) || strings.Contains(output, `"password"`) {
		t.Fatalf("unsafe JSON validation: %v %s", err, output)
	}
}

func TestConfigSyncSourcePushOnlyIgnoresInheritedPull(t *testing.T) {
	t.Setenv("PATH", "")
	backend := configSyncSourceBackendFixture(t, map[string]string{
		"bootstrap": `"mode" = "push_only"
["pull"]
"repository_id" = "inactive-pull"
["push"]
"repository_id" = "active-push"
`,
		"active-push": `"mode" = "push_only"
["pull"]
"repository_id" = "inactive-pull"
["push"]
"repository_id" = "active-push"
`,
	})
	projection, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, "bootstrap", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
	if err != nil || projection.PullRepositoryID != "" || projection.PushRepositoryID != "active-push" {
		t.Fatalf("push-only primary used inactive pull: projection=%+v error=%v", projection, err)
	}
	for _, id := range backend.calls {
		if id == "inactive-pull" {
			t.Fatal("inactive inherited pull was authorized or fetched")
		}
	}
	machine, _ := configsync.ParseSourceConfig([]byte(`"mode" = "push_only"
["pull"]
"repository_id" = "inactive-pull"
["push"]
"repository_id" = "active-push"
`), configsync.DefaultSourceConfigLimits())
	if _, err := buildConfigSyncProjection(context.Background(), backend, machine, "", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t)); err != nil {
		t.Fatal("inactive machine pull selected for bootstrap", err)
	}
}

func TestConfigSyncSourcePathInitJSON(t *testing.T) {
	root := shorttemp.Dir(t)
	previous := xdg.ConfigHome
	xdg.ConfigHome = root
	t.Cleanup(func() { xdg.ConfigHome = previous })
	for _, operation := range []string{"path", "init"} {
		command := newRootCommand()
		command.SetContext(context.Background())
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs([]string{"config", "sync", operation, "--json"})
		if err := command.Execute(); err != nil {
			t.Fatalf("%s JSON failed: %v %s", operation, err, output.String())
		}
		var envelope struct {
			OK            bool              `json:"ok"`
			SchemaVersion string            `json:"schema_version"`
			Data          map[string]string `json:"data"`
		}
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || !envelope.OK || envelope.SchemaVersion != "1.0" || envelope.Data["path"] == "" {
			t.Fatalf("%s did not return safe path envelope: %v %s", operation, err, output.String())
		}
	}
}

func TestConfigAssignmentAbsenceRequiresPureHTTPStatusCause(t *testing.T) {
	for _, fixture := range []struct {
		name string
		body string
		want bool
	}{
		{name: "valid not found response", body: `{"error":{"code":"not_found","message":"resource absent"}}`, want: true},
		{name: "malformed not found response", body: `{"error":`, want: false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(fixture.body))
			}))
			defer server.Close()
			client := api.New(server.URL, config.Credential{}, server.Client())
			_, err := client.ConfigAssignment(context.Background(), "machine_01")
			if err == nil || !api.IsNotFound(err) {
				t.Fatalf("fixture did not produce API not-found: %v", err)
			}
			if got := configAssignmentMissingOnly(err); got != fixture.want {
				t.Fatalf("absence proof=%v, want %v", got, fixture.want)
			}
			if fixture.want && configAssignmentMissingOnly(errors.Join(err, syscall.EIO)) {
				t.Fatal("joined operational sibling was hidden as an absent assignment")
			}
		})
	}
}

// An empty repository or a missing branch used to surface only as the generic
// "could not finish the command" message.
func TestConfigSyncProjectionExplainsEmptyRepositoryAndMissingBranch(t *testing.T) {
	t.Setenv("PATH", "")
	empty := filepath.Join(shorttemp.Dir(t), "empty.git")
	if _, err := git.PlainInit(empty, true); err != nil {
		t.Fatal(err)
	}
	backend := configSyncSourceBackendFixture(t, map[string]string{"branch": ""})
	backend.repos = append(backend.repos, api.ConfigRepository{ID: "empty", DisplayName: "empty", ExternalRef: empty, Provider: "git"})
	backend.grants["empty"] = configsync.RepositoryReadAccess{RepositoryID: "empty", CloneURL: empty, Branch: "main", Transport: "local", Capability: "repository_contents_read", ExpiresAt: time.Now().Add(5 * time.Minute)}
	missing := backend.grants["branch"]
	missing.Branch = "trunk"
	backend.grants["branch"] = missing
	for repository, want := range map[string]string{"empty": "no commits", "branch": "does not have the sync branch"} {
		_, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, repository, api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
		failure := classifyCommandFailure(err)
		if err == nil || failure.kind != commandRejected || !strings.Contains(commandFailureMessage(err, failure), want) {
			t.Fatalf("%s: kind=%v message=%q err=%v", repository, failure.kind, commandFailureMessage(err, failure), err)
		}
	}
}

// F11-02 device finding: with an empty machine file and no assignment, validate
// ended in the generic failure text because the missing selection was an
// unclassified error.
func TestConfigSyncProjectionWithoutARepositoryExplainsTheChoice(t *testing.T) {
	t.Setenv("PATH", "")
	backend := configSyncSourceBackendFixture(t, map[string]string{"one": ""})
	_, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, "", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	if err == nil || !strings.Contains(message, "--repository") || !strings.Contains(message, "pb config sync path") || strings.Contains(message, "could not finish") {
		t.Fatalf("message=%q err=%v", message, err)
	}
}

// A local repository is registered with the path it has on the machine that created it. Another
// machine of the account must be told it cannot reach it, not that Paperboat "could not finish".
func TestConfigSyncValidateNamesARepositoryThisMachineCannotReach(t *testing.T) {
	backend := configSyncSourceBackendFixture(t, map[string]string{"elsewhere": ""})
	if err := os.RemoveAll(backend.repos[0].ExternalRef); err != nil {
		t.Fatal(err)
	}
	_, err := buildConfigSyncProjection(context.Background(), backend, configsync.SourceConfig{}, "elsewhere", api.ConfigAssignment{}, configSyncMachineNames{}, shorttemp.Dir(t), "", shorttemp.Dir(t))
	var rejection commandRejection
	if !errors.As(err, &rejection) || rejection.reason != commandRejectConfigRepositoryUnavailable {
		t.Fatalf("error = %v, want the unreachable-repository refusal", err)
	}
	if message := rejection.Error(); !strings.Contains(message, "cannot reach the config repository") || strings.Contains(message, backend.repos[0].ExternalRef) {
		t.Fatalf("refusal = %q, want a message without the other machine's path", message)
	}
}
