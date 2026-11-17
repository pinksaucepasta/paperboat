package daemoncmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/cli/internal/recipekey"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// A recipe's sections are applied with pb's own commands, once: re-applying the same revision runs
// nothing, a removed preview is stopped, and a confirmation is answered with its code (F26-05).
func TestRecipeApplyEnsuresSectionsWithPBCommands(t *testing.T) {
	var ran []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		line := strings.Join(args, " ")
		ran = append(ran, line)
		switch {
		case args[0] == "preview" && args[1] != "stop":
			// pb preview --json prints the bare preview resource (F26-05 device check, rcp-l).
			return []byte(`{"schema":"paperboat.preview-tunnel/v1","kind":"preview_lease","id":"calm-summer-beach","state":"ready"}`), nil
		case args[0] == "tunnel" && args[1] == "create":
			// pb tunnel create --json prints the resource under "tunnel" (F26-05 device check, pbmac2).
			return []byte(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_create","tunnel":{"kind":"tunnel","id":"tunnel_1","name":"` + args[2] + `"}}`), nil
		case args[0] == "config" && !strings.Contains(line, "--confirm"):
			return []byte(`{"ok":false,"data":{"outcome":"confirmation_required","confirmation_token":"K3M9QX"}}`), errors.New("exit status 2")
		}
		return []byte(`{"ok":true,"data":{}}`), nil
	}
	source := filepath.Join(t.TempDir(), "paperboat", "config-sync.toml")
	a := &recipeApplier{statePath: filepath.Join(t.TempDir(), "recipe-apply.json"), machine: func() string { return "runner-7" }, machineSource: source, run: run}
	r := recipe.Recipe{Preview: []recipe.Preview{{Port: 3000, TTL: "2h"}}, Tunnel: []recipe.Tunnel{{Port: 8123, Private: true}},
		Config: &recipe.Config{Repository: "dotfiles"}, Env: &recipe.Env{Vaults: []string{"canary-env"}, Shell: true}}
	sections, err := a.apply(context.Background(), 1, r)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"preview 3000 --background --ttl 2h0m0s --json", "tunnel create runner-7 --json --port 8123 --private",
		"config sync apply --repository dotfiles --json", "config sync apply --repository dotfiles --json --confirm K3M9QX", "env hook --install"}
	if strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ran:\n%s", strings.Join(ran, "\n"))
	}
	for _, s := range []string{"preview", "tunnel", "config", "env"} {
		if v, ok := sections[s].(map[string]any); !ok || v["ok"] != true {
			t.Fatalf("section %s = %v", s, sections[s])
		}
	}

	// The recipe's [config] pulls only: the machine file is created so (PLAN F26), and a user's file stays.
	if data, err := os.ReadFile(source); err != nil || !strings.Contains(string(data), `mode = "pull_only"`) {
		t.Fatalf("machine source %q %v", data, err)
	}
	if err := os.WriteFile(source, []byte("mode = \"push_only\"\n"), 0o600); err != nil || ensurePullOnlySource(source) != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(source); string(data) != "mode = \"push_only\"\n" {
		t.Fatalf("an existing machine file was replaced: %q", data)
	}

	ran = nil
	if _, err := a.apply(context.Background(), 1, r); err != nil || len(ran) != 0 {
		t.Fatalf("re-apply ran %v (%v)", ran, err)
	}

	r.Preview = nil
	if _, err := a.apply(context.Background(), 2, r); err != nil || strings.Join(ran, "\n") != "preview stop calm-summer-beach --json" {
		t.Fatalf("removed preview: ran %v (%v)", ran, err)
	}
}

// A failing section is reported with pb's reason and fails the step, so it is retried.
func TestRecipeApplyReportsAFailedSection(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, error) {
		return []byte(`{"ok":false,"error":{"message":"preview limit reached"}}`), recipeCommandError(args, []byte(`{"ok":false,"error":{"message":"preview limit reached"}}`), nil, errors.New("exit status 3"))
	}
	a := &recipeApplier{statePath: filepath.Join(t.TempDir(), "s.json"), run: run}
	sections, err := a.apply(context.Background(), 1, recipe.Recipe{Preview: []recipe.Preview{{Port: 3000, TTL: "1h"}}})
	v, _ := sections["preview"].(map[string]any)
	if err == nil || v["error"] != "pb preview 3000: preview limit reached" {
		t.Fatalf("err=%v sections=%v", err, sections)
	}
	if again, _ := a.apply(context.Background(), 1, recipe.Recipe{Preview: []recipe.Preview{{Port: 3000, TTL: "1h"}}}); again["preview"] == nil {
		t.Fatal("a failed preview is tried again")
	}
}

// After a rotate the running machine takes the recipe's new key sealed to it and keeps reporting its
// own public key, so a later rotate can reach it too (F26: rotate keeps machines working).
func TestRecipeStateTakesARotatedKey(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	old, _ := recipe.NewToken("")
	f, _ := recipekey.New(old)
	if err := recipekey.Save(config, f); err != nil {
		t.Fatal(err)
	}
	next, _ := recipe.NewToken(old.ID)
	public, _ := f.MachinePublic()
	sealed, _ := recipe.SealRecipeKey(public, old.ID, next.PrivateKey())
	a := &recipeApplier{statePath: filepath.Join(dir, "s.json"), run: func(context.Context, ...string) ([]byte, error) { return nil, nil }}
	var reported []byte
	report := func(_ context.Context, _ int64, _ map[string]any, key []byte) error { reported = key; return nil }
	state := api.RecipeMachineState{Revision: 2, Recipe: []byte(`{"expires":"90d"}`), MachineKey: sealed}
	if err := applyRecipeState(context.Background(), a, config, state, report); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := recipekey.Load(config)
	if !bytes.Equal(saved.PrivateKey, next.PrivateKey()) || !bytes.Equal(reported, public) {
		t.Fatal("the rotated key was not taken or the machine key not reported")
	}
	if err := applyRecipeState(context.Background(), a, filepath.Join(t.TempDir(), "none.json"), state, report); err == nil {
		t.Fatal("a machine without a recipe key file applied a recipe")
	}
}

func TestRecipeStepErrorNamesARejectedRecipeSession(t *testing.T) {
	err := recipeStepError(&api.APIError{Status: http.StatusUnauthorized, Code: "unauthenticated"})
	if !strings.Contains(err.Error(), "recipe session") || strings.Contains(err.Error(), "pb login") && !strings.Contains(err.Error(), "recipe install") || !errors.Is(err, api.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
	if other := errors.New("x"); recipeStepError(other) != other {
		t.Fatal("other errors pass through")
	}
}

// A recipe tunnel is named like the machine (from the inventory cache), not like its hostname.
func TestRecipeMachineNameComesFromTheInventory(t *testing.T) {
	dir := t.TempDir()
	cache := `[{"id":"machine_2","alias":"other"},{"id":"machine_1","alias":"rcp-mu"}]`
	if err := os.WriteFile(filepath.Join(dir, localdaemon.InventoryCacheFile), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recipeMachineName(dir, "machine_1"); got != "rcp-mu" {
		t.Fatalf("name = %q", got)
	}
	host, _ := os.Hostname()
	if got := recipeMachineName(t.TempDir(), "machine_1"); got != strings.ToLower(strings.Split(host, ".")[0]) {
		t.Fatalf("fallback = %q", got)
	}
}
