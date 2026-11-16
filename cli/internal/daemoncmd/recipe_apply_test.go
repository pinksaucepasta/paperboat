package daemoncmd

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
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
			return []byte(`{"ok":true,"data":{"name":"calm-summer-beach"}}`), nil
		case args[0] == "tunnel" && args[1] == "create":
			return []byte(`{"ok":true,"data":{"name":"` + args[2] + `"}}`), nil
		case args[0] == "config" && !strings.Contains(line, "--confirm"):
			return []byte(`{"ok":false,"data":{"outcome":"confirmation_required","confirmation_token":"K3M9QX"}}`), errors.New("exit status 2")
		}
		return []byte(`{"ok":true,"data":{}}`), nil
	}
	a := &recipeApplier{statePath: filepath.Join(t.TempDir(), "recipe-apply.json"), machine: "runner-7", run: run}
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

func TestNotRecipeMachine(t *testing.T) {
	for status, want := range map[int]bool{http.StatusForbidden: true, http.StatusNotFound: true, http.StatusBadGateway: false} {
		if got := notRecipeMachine(&api.APIError{Status: status}); got != want {
			t.Fatalf("%d: %v", status, got)
		}
	}
}
