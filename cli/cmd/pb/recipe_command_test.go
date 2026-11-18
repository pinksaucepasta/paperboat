package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

const (
	recipeBase       = "/v1/workspaces/acme/recipes"
	recipeClientConf = `{"version":"1","machines_url":"https://console.example.test/machines","installer_base_url":"https://get.example.test"}`
	ciRecipeTOML     = "# CI runner\ntags = [\"ci\"]\nmax_machines = 20\nremove_after_offline = \"30m\"\n\n[[preview]]\nport = 3000\nttl = \"2h\"\n"
)

func recipeJSON(revision int, canonical string) string {
	return `{"id":"rcp_1","name":"ci","workspace":"acme","revision":` + itoa(revision) + `,"token":"pbr_k7f3q5xa2m_…","source":"# CI runner\n","format":"toml","recipe":` + canonical +
		`,"created_by":"user:ana@example.test","created_at":"2026-11-16T10:00:00Z","updated_at":"2026-11-16T10:00:00Z","expires_at":"2027-02-14T10:00:00Z","machines":3,"max_machines":20,"future":1}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func writeRecipeFile(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// create checks the file here, makes the token here (only id and auth are sent), prints it once
// with install lines from discovery (F26-01).
func TestRecipeCreateSendsIDAndAuthAndPrintsTheTokenOnce(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("POST "+recipeBase, 201, data(`{"recipe":`+recipeJSON(1, `{"tags":["ci"],"expires":"90d","max_machines":20,"remove_after_offline":"30m","preview":[{"port":3000,"ttl":"2h"}]}`)+`}`))
	fake.on("GET /v1/client-configuration", 200, data(recipeClientConf))
	code, out, errOut := runPolicyCLI(t, url, "recipe", "create", "-f", writeRecipeFile(t, "ci.toml", ciRecipeTOML))
	if code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	for _, want := range []string{"Recipe ci created in workspace acme.", "Tags        ci", "Reach       nothing", "Preview     port 3000 for 2h", "Machines    up to 20; removed 30m after going offline", "Expires     2027-02-14 (90d)", "curl -fsSL https://get.example.test | sh", "irm https://get.example.test | iex"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	sent := fake.sent("POST " + recipeBase)[0]
	tokenLine := out[strings.Index(out, "pbr_"):]
	token, err := recipe.ParseToken(strings.Fields(tokenLine)[0])
	if err != nil {
		t.Fatalf("printed token: %v", err)
	}
	if sent["name"] != "ci" || sent["format"] != "toml" || sent["source"] != ciRecipeTOML || sent["token_id"] != token.ID || sent["auth"] != base64.StdEncoding.EncodeToString(token.Auth()) {
		t.Fatalf("create body = %v", sent)
	}
	if body, _ := json.Marshal(sent); bytes.Contains(body, []byte(strings.Fields(tokenLine)[0][16:])) {
		t.Fatal("the token secret was sent")
	}
}

// A bad file is refused on this computer with file:line:col, before any request.
func TestRecipeCreateRefusesABadFileLocally(t *testing.T) {
	_, url := newPolicyFake(t)
	code, out, errOut := runPolicyCLI(t, url, "recipe", "create", "-f", writeRecipeFile(t, "ci.toml", "tag = [\"ci\"]\n"))
	if code == 0 || !strings.Contains(out+errOut, `ci.toml:1:1: unknown key "tag". Did you mean "tags"?`) || strings.Contains(out+errOut, "Usage:") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	code, out, errOut = runPolicyCLI(t, url, "recipe", "create", "-f", writeRecipeFile(t, "Bad Name.toml", "tags = []\n"))
	if code == 0 || !strings.Contains(out+errOut, "recipe name comes from the file name") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
}

// The server's refusals print with the names it sends (unknown vault with a suggestion).
func TestRecipeCreatePrintsServerRefusalsWithDetails(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+recipeBase[:len(recipeBase)-len("recipes")]+"env-vaults/canary", 404, apiFailure("not_found", `{}`))
	fake.on("POST "+recipeBase, 422, apiFailure("unknown_vault", `{"kind":"vault","name":"canary","known":["canary-env","prod-env"],"suggestion":"canary-env"}`))
	code, out, errOut := runPolicyCLI(t, url, "recipe", "create", "-f", writeRecipeFile(t, "ci.toml", "[env]\nvaults = [\"canary\"]\n"))
	if code == 0 || !strings.Contains(out+errOut, `vault "canary" does not exist in this workspace. Known: canary-env, prod-env. Did you mean "canary-env"?`) {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
}

func TestRecipeListShowAndUpdate(t *testing.T) {
	fake, url := newPolicyFake(t)
	fake.on("GET "+recipeBase, 200, data(`{"recipes":[`+recipeJSON(2, `{"tags":["ci"],"expires":"90d"}`)+`]}`))
	code, out, errOut := runPolicyCLI(t, url, "recipe", "list")
	if code != 0 || !strings.Contains(out, "NAME") || !strings.Contains(out, "ci") || !strings.Contains(out, "3 / 20") || !strings.Contains(out, "never") {
		t.Fatalf("list code=%d out=%s err=%s", code, out, errOut)
	}
	fake.on("GET "+recipeBase+"/ci", 200, data(`{"recipe":`+recipeJSON(2, `{}`)+`,"machines":[{"id":"m1","name":"fv-1","online":true,"applied_revision":2},{"id":"m2","name":"fv-2","online":true,"applied_revision":2,"apply_state":{"preview":{"error":"preview limit reached"}}}]}`))
	code, out, errOut = runPolicyCLI(t, url, "recipe", "show", "ci")
	if code != 0 || !strings.Contains(out, "Recipe ci, revision 2, token pbr_k7f3q5xa2m_…") || !strings.Contains(out, "# CI runner") || !strings.Contains(out, "Machines (2 of 20)") || !strings.Contains(out, "preview failed: preview limit reached") {
		t.Fatalf("show code=%d out=%s err=%s", code, out, errOut)
	}
	fake.on("PUT "+recipeBase+"/ci", 200, data(`{"recipe":`+recipeJSON(3, `{"preview":[{"port":3000,"ttl":"4h"}]}`)+`,"previous":`+recipeJSON(2, `{"preview":[{"port":3000,"ttl":"2h"}]}`)+`,"online_machines":2}`))
	code, out, errOut = runPolicyCLI(t, url, "recipe", "update", "-f", writeRecipeFile(t, "ci.toml", ciRecipeTOML))
	if code != 0 || !strings.Contains(out, "Recipe ci updated to revision 3: preview changed. 2 online machines will apply it now.") {
		t.Fatalf("update code=%d out=%s err=%s", code, out, errOut)
	}
}

// rotate and revoke print the impact and a confirmation code first; with the code they run (F26-01).
func TestRecipeRotateAndRevokeAskForConfirmation(t *testing.T) {
	fake, url := newPolicyFake(t)
	impact := `{"confirm":"K3M9QX","impact":{"machines":[{"id":"m1","name":"fv-1","online":true},{"id":"m2","name":"fv-2","online":false}],"vaults":[]}}`
	// The fake answers in order and repeats its last answer, so every answer is queued up front.
	machineKey := recipe.Token{Secret: make([]byte, 32)}.PublicKey() // a running machine's own key
	fake.on("GET "+recipeBase+"/ci", 200, data(`{"recipe":`+recipeJSON(2, `{}`)+`,"machines":[{"id":"m1","name":"fv-1","online":true,"public_key":"`+base64.StdEncoding.EncodeToString(machineKey)+`"},{"id":"m2","name":"fv-2"}]}`))
	fake.on("POST "+recipeBase+"/ci/rotate", 409, apiFailure("confirmation_required", impact))
	fake.on("POST "+recipeBase+"/ci/rotate", 200, data(`{"recipe":`+recipeJSON(2, `{}`)+`}`))
	fake.on("POST "+recipeBase+"/ci/revoke", 409, apiFailure("confirmation_required", strings.Replace(impact, "K3M9QX", "7RT2WD", 1)))
	fake.on("POST "+recipeBase+"/ci/revoke", 200, data(`{"recipe":`+recipeJSON(2, `{}`)+`,"impact":{"machines":[{"id":"m1","name":"fv-1"},{"id":"m2","name":"fv-2"}],"vaults":[]}}`))
	code, out, errOut := runPolicyCLI(t, url, "recipe", "rotate", "ci")
	if code != 0 || !strings.Contains(out, "Its 2 machines will keep running") || !strings.Contains(out, "Rotate with: pb recipe rotate ci --confirm K3M9QX") {
		t.Fatalf("rotate preview code=%d out=%s err=%s", code, out, errOut)
	}
	code, out, errOut = runPolicyCLI(t, url, "recipe", "rotate", "ci", "--confirm", "K3M9QX")
	if code != 0 || !strings.Contains(out, "Recipe ci rotated. New token (shown once):") {
		t.Fatalf("rotate code=%d out=%s err=%s", code, out, errOut)
	}
	token, err := recipe.ParseToken(strings.Fields(out[strings.Index(out, "pbr_"):])[0])
	sent := fake.sent("POST " + recipeBase + "/ci/rotate")[1]
	if err != nil || token.ID != "k7f3q5xa2m" || sent["confirm"] != "K3M9QX" || sent["auth"] != base64.StdEncoding.EncodeToString(token.Auth()) {
		t.Fatalf("rotate token=%v err=%v body=%v", token.ID, err, sent)
	}
	// The machine that reported its key gets the new key sealed to it; the other is told to re-pair.
	keys, _ := sent["machine_keys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["machine_id"] != "m1" || !strings.Contains(out, "1 machine did not report a key yet") {
		t.Fatalf("machine keys %v out=%s", sent["machine_keys"], out)
	}
	code, out, errOut = runPolicyCLI(t, url, "recipe", "revoke", "ci")
	if code != 0 || !strings.Contains(out, "its 2 machines will be removed: fv-1, fv-2") || !strings.Contains(out, "Revoke with: pb recipe revoke ci --confirm 7RT2WD") {
		t.Fatalf("revoke preview code=%d out=%s err=%s", code, out, errOut)
	}
	code, out, errOut = runPolicyCLI(t, url, "recipe", "revoke", "ci", "--confirm", "7RT2WD")
	if code != 0 || !strings.Contains(out, "Recipe ci revoked. 2 machines removed.") {
		t.Fatalf("revoke code=%d out=%s err=%s", code, out, errOut)
	}
}
