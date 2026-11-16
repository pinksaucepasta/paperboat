package recipes

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	store    *db.DB
	service  *Service
	device   *auth.DeviceService
	account  string
	web      string
	mu       sync.Mutex
	removed  []string
	personal workspace.Scope
}

func newFixture(t *testing.T) *fixture {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	f := &fixture{t: t, ctx: ctx, store: store, account: "rc_" + s, web: "mrcweb_" + s, personal: workspace.Scope{ID: "personal"}}
	f.service = New(store, audit.NewWriter(store), func(_ context.Context, account, machine string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.removed = append(f.removed, account+"/"+machine)
		return nil
	})
	f.device = auth.NewDeviceService(store, audit.NewWriter(store), config.Default().CLIAuth, []string{"recipe-test-token-hash-key"})
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, f.account)
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,last_seen_at)
	 VALUES($1,$2,$1,'web','linux','amd64','/w','online','occupied',true,now())`, f.web, f.account)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		for _, q := range []string{
			`DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`,
			`DELETE FROM paperboat.recipe_machines WHERE recipe_id IN (SELECT id FROM paperboat.recipes WHERE account_id=$1)`,
			`DELETE FROM paperboat.recipes WHERE account_id=$1`,
			`DELETE FROM paperboat.environment_named_vaults WHERE owner_account=$1`,
			`DELETE FROM paperboat.user_machines WHERE user_id=$1`,
			`DELETE FROM paperboat.workspace_policies WHERE owner_account=$1`,
			`DELETE FROM paperboat.users WHERE id=$1`,
		} {
			_, _ = store.SQL().ExecContext(c, q, f.account)
		}
	})
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) token() recipe.Token {
	f.t.Helper()
	tok, err := recipe.NewToken("")
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func (f *fixture) session(tok recipe.Token) (auth.RecipeSession, error) {
	return f.device.StartRecipeSession(f.ctx, auth.RecipeSessionRequest{TokenID: tok.ID, Auth: tok.Auth(), Label: "runner", DeviceType: "container", OS: "linux"})
}

func refusal(t *testing.T, err error, code string) {
	t.Helper()
	var r *auth.RecipeRefusal
	if !errors.As(err, &r) || r.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestRecipeLifecycle(t *testing.T) {
	f := newFixture(t)
	ci := "# CI\ntags = [\"ci\"]\nmax_machines = 1\nremove_after_offline = \"10m\"\n\n[[reach]]\ndst = [\"machine:web\"]\nallow = [\"exec\"]\n"
	// An agent recipe may reach tag:ci before any machine carries it (the CI recipe comes later).
	agent := f.token()
	if _, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "agent", Source: "max_machines = 1\n\n[[reach]]\ndst = [\"tag:ci\"]\nallow = [\"exec\"]\n", Format: "toml", TokenID: agent.ID, Auth: agent.Auth(), PublicKey: agent.PublicKey()}); err != nil {
		t.Fatalf("reach to a tag no machine carries yet: %v", err)
	}
	tok := f.token()
	created, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: ci, Format: "toml", TokenID: tok.ID, Auth: tok.Auth(), PublicKey: tok.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.Recipe.MaxMachines != 1 || !strings.Contains(created.Source, "# CI") || created.ExpiresAt.Sub(created.CreatedAt) != recipe.DefaultExpires {
		t.Fatalf("created %+v", created)
	}
	// A lost response retried with the same token returns the same recipe; another token is a name clash.
	if again, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: ci, Format: "toml", TokenID: tok.ID, Auth: tok.Auth(), PublicKey: tok.PublicKey()}); err != nil || again.ID != created.ID {
		t.Fatalf("retry: %v", err)
	}
	other := f.token()
	if _, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: ci, Format: "toml", TokenID: other.ID, Auth: other.Auth(), PublicKey: other.PublicKey()}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	var file *FileError
	if _, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "bad", Source: "tag = [\"x\"]\n", Format: "toml", TokenID: other.ID, Auth: other.Auth(), PublicKey: other.PublicKey()}); !errors.As(err, &file) || !strings.Contains(err.Error(), `Did you mean "tags"?`) {
		t.Fatalf("file error: %v", err)
	}
	var reach *ReachError
	if _, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "agent", Source: "[[reach]]\ndst = [\"machine:nope\"]\nallow = [\"exec\"]\n", Format: "toml", TokenID: other.ID, Auth: other.Auth(), PublicKey: other.PublicKey()}); !errors.As(err, &reach) {
		t.Fatalf("unknown reach target: %v", err)
	}

	// Pairing: a wrong auth is invalid, the right one is a session of the creator carrying the recipe.
	refusal(t, func() error { _, err := f.session(other); return err }(), "recipe_invalid")
	s, err := f.session(tok)
	if err != nil || s.RecipeID != created.ID || s.Revision != 1 {
		t.Fatalf("session: %+v %v", s, err)
	}
	var recipeID string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT recipe_id FROM paperboat.cli_client_sessions WHERE id=$1`, s.Tokens.CLIClientSessionID).Scan(&recipeID); err != nil || recipeID != created.ID {
		t.Fatalf("session recipe %q %v", recipeID, err)
	}
	// max_machines counts live machines of the recipe.
	f.exec(`INSERT INTO paperboat.recipe_machines(machine_id,recipe_id) VALUES($1,$2)`, f.web, created.ID)
	refusal(t, func() error { _, err := f.session(tok); return err }(), "recipe_full")

	// Update raises the revision, keeps the token and tells the recipe's machines at once.
	moved := f.topicWatch(f.web)
	before, after, err := f.service.Update(f.ctx, f.account, f.personal, Input{Name: "ci", Source: strings.Replace(ci, "max_machines = 1", "max_machines = 5", 1), Format: "toml"})
	if err != nil || before.Revision != 1 || after.Revision != 2 || after.TokenID != created.TokenID || after.MaxMachines != 5 {
		t.Fatalf("update: %+v %v", after, err)
	}
	moved()

	// Rotate needs the shown code; the old token is then "replaced", the new one pairs.
	next := recipe.Token{ID: tok.ID, Secret: f.token().Secret}
	var confirm *ConfirmError
	if _, err := f.service.Rotate(f.ctx, f.account, f.personal, "ci", next.Auth(), next.PublicKey(), nil, nil, ""); !errors.As(err, &confirm) || len(confirm.Impact.Machines) != 1 {
		t.Fatalf("rotate without code: %v", err)
	}
	if _, err := f.service.Rotate(f.ctx, f.account, f.personal, "ci", next.Auth(), next.PublicKey(), nil, []MachineKey{{MachineID: f.web, Sealed: []byte("sealed")}}, confirm.Code); err != nil {
		t.Fatal(err)
	}
	refusal(t, func() error { _, err := f.session(tok); return err }(), "recipe_replaced")
	if _, err := f.session(next); err != nil {
		t.Fatalf("new token: %v", err)
	}
	if state, err := f.service.MachineRecipe(f.ctx, created.ID, f.web); err != nil || string(state.MachineKey) != "sealed" || state.Revision != 2 {
		t.Fatalf("machine recipe: %+v %v", state, err)
	}
	f.exec(`UPDATE paperboat.recipes SET created_at=now()-interval '2 hours', expires_at=now()-interval '1 minute' WHERE id=$1`, created.ID)
	refusal(t, func() error { _, err := f.session(next); return err }(), "recipe_expired")

	// Revoke: code, sessions end, machines removed.
	if _, _, err := f.service.Revoke(f.ctx, f.account, f.personal, "ci", "wrong"); !errors.As(err, &confirm) {
		t.Fatalf("revoke without code: %v", err)
	}
	if _, impact, err := f.service.Revoke(f.ctx, f.account, f.personal, "ci", confirm.Code); err != nil || len(impact.Machines) != 1 {
		t.Fatalf("revoke: %v", err)
	}
	var live int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.cli_client_sessions WHERE recipe_id=$1 AND state='active'`, created.ID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("sessions after revoke: %d %v", live, err)
	}
	if len(f.removed) != 1 || f.removed[0] != f.account+"/"+f.web {
		t.Fatalf("removed %v", f.removed)
	}
	if _, err := f.service.List(f.ctx, f.account, f.personal); err != nil {
		t.Fatal(err)
	}
}

func TestRecipeVaults(t *testing.T) {
	f := newFixture(t)
	wraps := []Wrap{{Kind: "account", ID: f.account, Wrapped: []byte("member-wrap")}}
	if _, err := f.service.CreateVault(f.ctx, f.account, f.personal, "canary-env", wraps); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PutValues(f.ctx, f.account, f.personal, "canary-env", 0, 1, []byte("values-1")); err != nil {
		t.Fatal(err)
	}
	tok := f.token()
	var name *NameError
	_, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: "[env]\nvaults = [\"canary\"]\n", Format: "toml", TokenID: tok.ID, Auth: tok.Auth(), PublicKey: tok.PublicKey()})
	if !errors.As(err, &name) || name.Suggestion != "canary-env" {
		t.Fatalf("unknown vault: %v", err)
	}
	var missing *WrapMissingError
	if _, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: "[env]\nvaults = [\"canary-env\"]\n", Format: "toml", TokenID: tok.ID, Auth: tok.Auth(), PublicKey: tok.PublicKey()}); !errors.As(err, &missing) {
		t.Fatalf("no wrap: %v", err)
	}
	created, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: "[env]\nvaults = [\"canary-env\"]\n", Format: "toml", TokenID: tok.ID, Auth: tok.Auth(), PublicKey: tok.PublicKey(),
		Wraps: []VaultWrap{{Vault: "canary-env", Wrapped: []byte("recipe-wrap")}}})
	if err != nil {
		t.Fatal(err)
	}
	// A value change moves the recipe topic of the machines that read the vault.
	f.exec(`INSERT INTO paperboat.recipe_machines(machine_id,recipe_id) VALUES($1,$2)`, f.web, created.ID)
	moved := f.topicWatch(f.web)
	if _, err := f.service.PutValues(f.ctx, f.account, f.personal, "canary-env", 1, 1, []byte("values-1b")); err != nil {
		t.Fatal(err)
	}
	moved()
	// The machine reads the vault with nobody online.
	state, err := f.service.MachineRecipe(f.ctx, created.ID, "")
	if err != nil || len(state.Vaults) != 1 || string(state.Vaults[0].Envelope) != "values-1b" || string(state.Vaults[0].Wrapped) != "recipe-wrap" {
		t.Fatalf("machine vaults: %+v %v", state, err)
	}
	if err := f.service.DeleteVault(f.ctx, f.account, f.personal, "canary-env"); !errors.Is(err, ErrVaultInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	// Revoking the recipe drops its wrap; the vault keeps its key and members keep writing (user rule:
	// removing one recipe never affects anyone else).
	var confirm *ConfirmError
	_, _, err = f.service.Revoke(f.ctx, f.account, f.personal, "ci", "")
	if !errors.As(err, &confirm) {
		t.Fatal(err)
	}
	if _, _, err := f.service.Revoke(f.ctx, f.account, f.personal, "ci", confirm.Code); err != nil {
		t.Fatal(err)
	}
	if v, err := f.service.GetVault(f.ctx, f.account, f.personal, "canary-env"); err != nil || len(v.Recipes) != 0 || v.KeyEpoch != 1 {
		t.Fatalf("after revoke: %+v %v", v, err)
	}
	if _, err := f.service.Rekey(f.ctx, f.account, f.personal, "canary-env", 2, []byte("values-2"), wraps, nil); !errors.Is(err, ErrVaultStale) {
		t.Fatalf("stale rekey: %v", err)
	}
	v, err := f.service.Rekey(f.ctx, f.account, f.personal, "canary-env", 1, []byte("values-2"), wraps, nil)
	if err != nil || v.KeyEpoch != 2 || len(v.Recipes) != 0 || string(v.Wraps[0].Wrapped) != "member-wrap" {
		t.Fatalf("rekey: %+v %v", v, err)
	}
	if err := f.service.DeleteVault(f.ctx, f.account, f.personal, "canary-env"); err != nil {
		t.Fatal(err)
	}
}

func TestCollectMachines(t *testing.T) {
	f := newFixture(t)
	tok := f.token()
	created, err := f.service.Create(f.ctx, f.account, f.personal, Input{Name: "ci", Source: "remove_after_offline = \"10m\"\n", Format: "toml", TokenID: tok.ID, Auth: tok.Auth(), PublicKey: tok.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	gone := "mrcgone_" + f.account
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,last_seen_at)
	 VALUES($1,$2,$1,'gone','linux','amd64','/w','offline','occupied',false,now()-interval '20 minutes')`, gone, f.account)
	f.exec(`INSERT INTO paperboat.recipe_machines(machine_id,recipe_id) VALUES($1,$3),($2,$3)`, f.web, gone, created.ID)
	if err := f.service.CollectMachines(f.ctx); err != nil {
		t.Fatal(err)
	}
	// The online machine (a live session) stays; the one offline for longer than 10m goes.
	if len(f.removed) != 1 || f.removed[0] != f.account+"/"+gone {
		t.Fatalf("removed %v", f.removed)
	}
	var scheduled bool
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT offline_remove_at IS NOT NULL FROM paperboat.recipe_machines WHERE machine_id=$1`, f.web).Scan(&scheduled); err != nil || scheduled {
		t.Fatalf("an online machine was scheduled for removal: %v", err)
	}
	// machine_lifetime removes a machine even while it is online.
	f.exec(`UPDATE paperboat.recipe_machines SET remove_at=now()-interval '1 second' WHERE machine_id=$1`, f.web)
	if err := f.service.CollectMachines(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 3 || f.removed[2] != f.account+"/"+f.web {
		t.Fatalf("lifetime removal: %v", f.removed)
	}
}

// topicWatch reads machine's recipe topic generation now; the returned check fails unless it moved.
func (f *fixture) topicWatch(machine string) func() {
	f.t.Helper()
	read := func() int64 {
		var g int64
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='recipe'),0)`, machine).Scan(&g); err != nil {
			f.t.Fatal(err)
		}
		return g
	}
	before := read()
	return func() {
		f.t.Helper()
		if after := read(); after <= before {
			f.t.Fatalf("recipe topic of %s did not move (%d -> %d)", machine, before, after)
		}
	}
}
