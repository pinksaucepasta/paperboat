package recordings

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

const testIssuer = "https://api.example.test"

type fixture struct {
	t                        *testing.T
	ctx                      context.Context
	store                    *db.DB
	s                        *Service
	signer                   *mint.Provider
	teams                    *teams.Service
	team                     string
	owner, admin, member, bo string
	prod, dev                string
	sharing                  map[string][]byte
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
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	x := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	f := &fixture{t: t, ctx: ctx, store: store, signer: signer, teams: teams.NewService(store), team: "rec-" + x,
		owner: "ro_" + x, admin: "ra_" + x, member: "rm_" + x, bo: "rb_" + x, prod: "mprod_" + x, dev: "mdev_" + x, sharing: map[string][]byte{}}
	cfg := config.Recordings{MachineRetentionDays: 30, DiskQuotaMB: 2048, MaxKeyGenerations: 3}
	f.s = New(store, cfg, signer, testIssuer)
	f.s.sharingKey = func(_ context.Context, _ *db.Tx, account string) ([]byte, error) { return f.sharing[account], nil }
	users := []string{f.owner, f.admin, f.member, f.bo}
	for _, u := range users {
		f.sharing[u] = random(t, 32)
	}
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT u,u,u||'@example.test','active' FROM unnest($1::text[]) u`, users)
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team, f.owner)
	for _, u := range users {
		f.exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'recordings test','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, "cli_"+u, u)
	}
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'admin',true),($1,$4,1,'member',true),($1,$5,1,'member',true)`, f.team, f.owner, f.admin, f.member, f.bo)
	for _, m := range [][2]string{{f.prod, "prod"}, {f.dev, "dev"}} {
		f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id) VALUES($1,$2,$1,$3,'linux','amd64','/w','online','occupied',true,$4)`, m[0], f.owner, m[1], f.team)
		f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, f.team, m[0], f.owner)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.user_machines WHERE id=ANY($1)`, []string{f.prod, f.dev})
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.teams WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.cli_client_sessions WHERE user_id=ANY($1)`, users)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.users WHERE id=ANY($1)`, users)
	})
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) paid() {
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',4,now()-interval '1 hour',now()+interval '1 hour')`, "bw-"+f.team, f.team)
}

func (f *fixture) policy(source string) error {
	f.t.Helper()
	cur, err := f.teams.Policy(f.ctx, f.owner, f.team, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	req := teams.PolicySaveRequest{Policy: source, ExpectedVersion: cur.Version}
	_, err = f.teams.SavePolicy(f.ctx, f.owner, f.team, req)
	var unconfirmed *teams.PolicyUnconfirmedError
	if errors.As(err, &unconfirmed) {
		req.Confirm = unconfirmed.Confirm
		_, err = f.teams.SavePolicy(f.ctx, f.owner, f.team, req)
	}
	return err
}

func (f *fixture) machinePolicy(machine string) recording.Policy {
	f.t.Helper()
	p, err := f.s.MachinePolicy(f.ctx, machine)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) topic(machine string) int64 {
	var g int64
	_ = f.store.SQL().QueryRowContext(f.ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='recording_policy'),0)`, machine).Scan(&g)
	return g
}

func random(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newKey(t *testing.T, team string, gen int64, previous ed25519.PrivateKey) (recording.Key, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := recording.Key{Generation: gen, PublicKey: random(t, 32), SigningPublicKey: pub}
	if previous != nil {
		k.RotationSignature = ed25519.Sign(previous, recording.RotationMessage(team, gen, k.PublicKey, k.SigningPublicKey))
	}
	return k, priv
}

// native is a CLI replay request from the member's client session.
func (f *fixture) native() recording.ReplayRequest {
	return recording.ReplayRequest{CLIClientSessionID: "cli_" + f.member}
}

func (f *fixture) grant(account string) recording.Grant {
	return recording.Grant{AccountID: account, SharingPublicKey: f.sharing[account], Sealed: sealed(account)}
}

const recID = "rec_01234567-89ab-cdef-0123-456789abcdef"

// The whole server side of a recording: the Teams gate, who is recorded where, the key's pin and signed rotation,
// the index fed by the machine, the list, and replay only with permission, a sealed copy and the audit event first.
func TestPostgresRecordingsEndToEnd(t *testing.T) {
	f := newFixture(t)
	source := `{"tags": {"tag:prod": ["machine:prod"]}, "rules": [
		{"src": ["role:member"], "dst": ["tag:prod"], "allow": ["record"]},
		{"src": ["user:` + f.bo + `@example.test"], "dst": ["tag:prod"], "deny": ["record"]},
		{"src": ["user:` + f.member + `@example.test"], "dst": ["machine:prod"], "allow": ["recording_view"]}]}`

	// Not on the Teams plan: recording rules are refused and machines record nothing.
	var invalid *teams.PolicyInvalidError
	if err := f.policy(source); !errors.As(err, &invalid) || invalid.Errors[0].Code != "team_plan_required" {
		t.Fatalf("policy without Teams = %v, want team_plan_required", err)
	}
	if p := f.machinePolicy(f.prod); p.Enabled || len(p.RecordAccounts) != 0 {
		t.Fatalf("unpaid policy = %+v", p)
	}
	if _, err := f.s.KeyState(f.ctx, f.owner, f.team); !errors.Is(err, ErrNeedsTeam) {
		t.Fatalf("KeyState unpaid = %v", err)
	}

	f.paid()
	if err := f.policy(source); err != nil {
		t.Fatal(err)
	}
	p := f.machinePolicy(f.prod)
	if !p.Valid() {
		t.Fatalf("server policy fails the client check: %+v", p)
	}
	// role:member covers every member; bo's deny exempts them.
	if want := []string{f.admin, f.member, f.owner}; !p.Enabled || p.TeamID != f.team || !slices.Equal(p.RecordAccounts, sorted(want)) || len(p.Keys) != 0 {
		t.Fatalf("policy = %+v, want record %v", p, want)
	}
	if !slices.Equal(p.AdminAccounts, sorted([]string{f.owner, f.admin})) || p.ManualStart != recording.ManualStartAnyone || p.RetentionDays != 30 {
		t.Fatalf("policy = %+v", p)
	}
	if d := f.machinePolicy(f.dev); len(d.RecordAccounts) != 0 {
		t.Fatalf("dev is not tagged prod: %+v", d)
	}

	// Key generation 1: admins only, sealed for at least one viewer; viewers are who recording_view names.
	k1, s1 := newKey(t, f.team, 1, nil)
	if _, err := f.s.CreateKey(f.ctx, f.member, f.team, recording.KeyCreate{Key: k1, Grants: []recording.Grant{f.grant(f.member)}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member CreateKey = %v", err)
	}
	if _, err := f.s.CreateKey(f.ctx, f.owner, f.team, recording.KeyCreate{Key: k1, Grants: []recording.Grant{f.grant(f.bo)}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant to a non-viewer = %v", err)
	}
	before := f.topic(f.prod)
	if _, err := f.s.CreateKey(f.ctx, f.owner, f.team, recording.KeyCreate{Key: k1, Grants: []recording.Grant{f.grant(f.member)}}); err != nil {
		t.Fatal(err)
	}
	if f.topic(f.prod) <= before {
		t.Fatal("creating the key did not move the machines' recording_policy topic")
	}
	if v1 := f.machinePolicy(f.prod); len(v1.Keys) != 1 || v1.Version == p.Version {
		t.Fatalf("policy after key = %+v", v1)
	}
	// A rotation not signed by the current key is refused; a signed one is accepted with the previous key sealed.
	forged, _ := newKey(t, f.team, 2, nil)
	forged.RotationSignature = random(t, 64)
	if _, err := f.s.CreateKey(f.ctx, f.owner, f.team, recording.KeyCreate{Key: forged, SealedPrevious: sealed("p"), Grants: []recording.Grant{f.grant(f.member)}}); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("forged rotation = %v", err)
	}
	k2, _ := newKey(t, f.team, 2, s1)
	if _, err := f.s.CreateKey(f.ctx, f.owner, f.team, recording.KeyCreate{Key: k2, SealedPrevious: sealed("gen1-sealed-to-gen2"), Grants: []recording.Grant{f.grant(f.member)}}); err != nil {
		t.Fatal(err)
	}
	if keys := f.machinePolicy(f.prod).Keys; len(keys) != 2 || !recording.VerifyRotation(f.team, keys[0], keys[1]) {
		t.Fatalf("policy keys = %+v", keys)
	}

	// The machine reports a recording started with generation 1 (it had not seen the rotation yet).
	now := time.Now().UTC()
	apply := func(machine, kind string, r recording.Report) {
		t.Helper()
		if err := r.Validate(kind, now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.InTx(f.ctx, func(ctx context.Context, tx *db.Tx) error { return ApplyReportTx(ctx, tx, machine, kind, r) }); err != nil {
			t.Fatal(err)
		}
	}
	started := recording.Report{RecordingID: recID, SessionID: "sess_1", Trigger: recording.TriggerRule, KeyGeneration: 1, Accounts: []string{f.member}, StartedAt: now}
	apply(f.prod, recording.KindStarted, started)
	apply(f.prod, recording.KindStarted, started) // a retried upload changes nothing
	apply(f.dev, recording.KindStopped, recording.Report{RecordingID: recID, Reason: recording.ReasonError, EndedAt: now})
	apply(f.prod, recording.KindStopped, recording.Report{RecordingID: recID, Reason: recording.ReasonSessionEnded, Accounts: []string{f.admin}, Bytes: 4096, Segments: 2, EndedAt: now})

	list, err := f.s.List(f.ctx, f.admin, f.team, Filter{MachineID: f.prod})
	if err != nil || list.Total != 1 || len(list.Recordings) != 1 {
		t.Fatalf("admin list = %+v, %v", list, err)
	}
	r := list.Recordings[0]
	if r.State != recording.StateEnded || r.Bytes != 4096 || r.Segments != 2 || r.MachineName != "prod" || !slices.Equal(r.Accounts, sorted([]string{f.admin, f.member})) {
		t.Fatalf("recording = %+v (another machine's stop must not apply)", r)
	}
	if l, err := f.s.List(f.ctx, f.bo, f.team, Filter{}); err != nil || l.Total != 0 {
		t.Fatalf("bo without recording_view sees %+v, %v", l, err)
	}
	if l, err := f.s.List(f.ctx, f.member, f.team, Filter{}); err != nil || l.Total != 1 {
		t.Fatalf("viewer list = %+v, %v", l, err)
	}

	// Replay: only with recording_view (owner and admins too), only with a copy sealed to the current vault.
	if _, err := f.s.Replay(f.ctx, f.admin, recID, f.native()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin without a rule replays: %v", err)
	}
	f.sharing[f.member] = random(t, 32) // the member reset their vault: the old copy no longer opens
	if _, err := f.s.Replay(f.ctx, f.member, recID, f.native()); !errors.Is(err, ErrKeyPending) {
		t.Fatalf("stale copy = %v", err)
	}
	if err := f.s.PutGrants(f.ctx, f.member, f.team, recording.GrantsPut{Generation: 2, Grants: []recording.Grant{f.grant(f.member)}}); err != nil {
		t.Fatal(err)
	}
	out, err := f.s.Replay(f.ctx, f.member, recID, f.native())
	if err != nil {
		t.Fatal(err)
	}
	if out.MachineID != f.prod || out.KeyGeneration != 1 || out.ViewerKey.Generation != 2 || len(out.Chain) != 1 || string(out.Chain[0].SealedPrevious) != string(sealed("gen1-sealed-to-gen2")) || out.TeamID != f.team {
		t.Fatalf("replay = %+v", out)
	}
	claims, err := f.signer.VerifyCredential(out.Ticket, testIssuer, recording.CredentialClass, time.Now())
	if err != nil || claims.MachineID != f.prod || claims.AccountID != f.member || claims.WorkspaceID != f.team || claims.ResourceID != recID || claims.ResourceKind != recording.ResourceKind ||
		claims.CLIClientSessionID != "cli_"+f.member {
		t.Fatalf("ticket claims = %+v, %v", claims, err)
	}
	var audited int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='team.recording_replayed' AND resource_id=$1 AND actor_user_id=$2 AND metadata->>'recording_id'=$3 AND idempotency_key=$2||':'||$4`, f.team, f.member, recID, claims.JTI).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("replay audit events = %d, %v", audited, err)
	}

	// The CLI endpoint is admitted to the machine for the ticket's lifetime under the ticket's jti.
	var admitted int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.recording_native_admissions WHERE id=$1 AND native_cli_session_id=$2 AND machine_id=$3 AND expires_at>now()`, claims.JTI, "cli_"+f.member, f.prod).Scan(&admitted); err != nil || admitted != 1 {
		t.Fatalf("native admissions = %d, %v", admitted, err)
	}
	if _, err := f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{CLIClientSessionID: "cli_" + f.bo}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("replay through another account's client = %v", err)
	}
	if _, err := f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{CLIClientSessionID: "cli_" + f.member, BrowserPublicKeySHA256: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("both forms at once = %v", err)
	}
	// The browser form returns the key material without a machine ticket (the browser ticket comes from the gateway),
	// after its own audit event.
	browser, err := f.s.Replay(f.ctx, f.member, recID, recording.ReplayRequest{BrowserPublicKeySHA256: "browser-key-digest"})
	if err != nil || browser.Ticket != "" || browser.ViewerKey.Generation != 2 {
		t.Fatalf("browser replay = %+v, %v", browser, err)
	}
	if team, machine, err := f.s.ResolveReplay(f.ctx, f.member, recID); err != nil || team != f.team || machine != f.prod {
		t.Fatalf("ResolveReplay = %q %q %v", team, machine, err)
	}
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='team.recording_replayed' AND resource_id=$1 AND actor_user_id=$2`, f.team, f.member).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("replay audit events = %d, %v", audited, err)
	}
	// Losing recording_view removes the copy (history is not re-encrypted); deletion on the machine ends replay.
	if err := f.policy(`{"rules": [{"src": ["role:member"], "dst": ["machine:*"], "allow": ["record"]}]}`); err != nil {
		t.Fatal(err)
	}
	var grants int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.recording_key_grants WHERE team_id=$1`, f.team).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("grants after losing recording_view = %d, %v", grants, err)
	}
	if _, err := f.s.Replay(f.ctx, f.member, recID, f.native()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("replay after losing the rule = %v", err)
	}
	apply(f.prod, recording.KindDeleted, recording.Report{RecordingID: recID, Reason: recording.ReasonRetention})
	if l, _ := f.s.List(f.ctx, f.owner, f.team, Filter{SessionID: "sess_1"}); l.Total != 1 || l.Recordings[0].State != recording.StateDeleted {
		t.Fatalf("deleted recording = %+v", l)
	}

	// Admins-only manual start, and a role change, move the topic.
	before = f.topic(f.prod)
	if err := f.s.PutSettings(f.ctx, f.admin, f.team, recording.Settings{ManualStart: recording.ManualStartAdmins}); err != nil {
		t.Fatal(err)
	}
	if p := f.machinePolicy(f.prod); p.ManualStart != recording.ManualStartAdmins || f.topic(f.prod) <= before {
		t.Fatalf("settings not applied or not published: %+v", p)
	}
	before = f.topic(f.prod)
	f.exec(`UPDATE paperboat.team_members SET role='admin' WHERE team_id=$1 AND account_id=$2`, f.team, f.bo)
	if f.topic(f.prod) <= before || !slices.Contains(f.machinePolicy(f.prod).AdminAccounts, f.bo) {
		t.Fatal("a role change did not reach the machines")
	}
}

// sealed is a stand-in sealed bundle of the real size, marked with who it is for.
func sealed(mark string) []byte {
	out := make([]byte, recording.SealedBytes)
	copy(out, mark)
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
