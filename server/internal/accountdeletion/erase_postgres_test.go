package accountdeletion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

type fixture struct {
	t      *testing.T
	store  *db.DB
	writer *audit.Writer
	svc    *Service
	suffix string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	writer := audit.NewWriter(store)
	machines := usermachines.New(store, writer, usermachines.Policy{}, nil)
	mail := config.TransactionalMail{From: "sender@example.test", ConsoleOrigin: "https://console.example.test"}
	return &fixture{t: t, store: store, writer: writer, svc: New(store, machines, mail), suffix: fmt.Sprint(time.Now().UnixNano())}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.t.Context(), query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.SQL().QueryRowContext(f.t.Context(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
	return n
}

func (f *fixture) id(kind string) string {
	return kind + "_" + f.suffix + "_" + fmt.Sprint(time.Now().UnixNano())
}

func digest(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// user creates an active account whose email is name@example.test.
func (f *fixture) user(name string) auth.User {
	id := name + "_" + f.suffix
	email := id + "@example.test"
	f.exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, id, "subj_"+id, email)
	return auth.User{ID: id, WorkOSSubject: "subj_" + id, PrimaryEmail: email, Status: "active"}
}

// seedPersonal gives the account one of each personal thing the deletion must handle.
func (f *fixture) seedPersonal(u auth.User) (machine string) {
	f.t.Helper()
	sessionID := "sess_" + u.ID
	f.exec(`INSERT INTO sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,'csrf',now()+interval '1 day')`, sessionID, u.ID, hex.EncodeToString(digest(sessionID)))
	cli := "cli_" + u.ID
	f.exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'pb','laptop','linux','linux',ARRAY['account:read'],'active',now(),now())`, cli, u.ID)
	f.exec(`INSERT INTO cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES($1,$2,'active',now()+interval '1 day',now())`, "rt_"+u.ID, cli)
	f.exec(`INSERT INTO cli_access_tokens(token_hash,cli_client_session_id,expires_at,created_at) VALUES($1,$2,now()+interval '1 hour',now())`, "at_"+u.ID, cli)
	f.exec(`INSERT INTO api_tokens(id,account_id,name,secret_hash,expires_at) VALUES($1,$2,'ci',$3,now()+interval '1 day')`, "tok_"+u.ID, u.ID, digest("tok_"+u.ID))
	machine = "machine_" + u.ID
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'m-'||md5($1),'linux','amd64','/workspace','online','occupied',true,1)`, machine, u.ID)
	f.exec(`INSERT INTO user_favorites(user_id,kind,resource_id) VALUES($1,'machine',$2)`, u.ID, machine)
	f.exec(`INSERT INTO user_identities(user_id,provider,provider_subject,email) VALUES($1,'workos',$2,$3)`, u.ID, u.WorkOSSubject, u.PrimaryEmail)
	f.exec(`INSERT INTO account_e2ee_keys(key_id,user_id,public_key,fingerprint) VALUES($1,$2,$3,$3)`, e2eeKeyID(u), u.ID, digest("pk_"+u.ID))
	f.exec(`INSERT INTO environment_vault_scopes(owner_kind,owner_id,machine_id,key_epoch,revision,document_id,envelope,writer_public,workspace_id)
		VALUES('personal',$1,'',1,1,$2,'\x01',$3,'personal')`, u.ID, "sha256:"+hex.EncodeToString(digest("doc_"+u.ID)), digest("writer_"+u.ID))
	f.exec(`INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline,allocation_state,edge_state,origin_state)
		VALUES($1,'https://preview-'||md5($1)||'.example.test',$2,$2,'dev','sess','foreground','http','127.0.0.1:3000',now()+interval '1 hour','ready','ready','ready')`, "prev_"+u.ID, u.ID)
	f.exec(`INSERT INTO tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id)
		VALUES($1,$2,'docs',$3,'https://tunnel-'||md5($1)||'.example.test',$4,$2)`, "tun_"+u.ID, u.ID, "endpoint_"+uuidFor(u.ID), machine)
	f.exec(`INSERT INTO billing_workspaces(id,user_id,provider_subscription_id,state) VALUES($1,$2,$3,'active')`, "bw_"+u.ID, u.ID, "sub_"+u.ID)
	f.exec(`INSERT INTO peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,signer_public_key,issued_at,expires_at,key_id)
		VALUES($1,$2,$3,'cli',1,1,decode(repeat('00',200),'hex'),$4,$5,now(),now()+interval '1 day',$6)`, digest("quic_"+u.ID), u.ID, "peer_"+u.ID, digest("qpk_"+u.ID), digest("signer_"+u.ID), e2eeKeyID(u))
	f.exec(`INSERT INTO peer_network_identities(user_id,endpoint_id,role,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
		VALUES($1,$2,'cli',1,0,1,$3,$4,$5::inet,now(),now())`, u.ID, "peer_"+u.ID, digest("wg_"+u.ID), digest("quic_"+u.ID), f.virtualAddress())
	if err := f.writer.Write(f.t.Context(), audit.Event{ActorUserID: u.ID, ActorType: audit.ActorUser, EventType: "test.personal", ResourceType: "user_machine", ResourceID: machine, Metadata: map[string]any{}}); err != nil {
		f.t.Fatal(err)
	}
	return machine
}

// e2eeKeyID is the account's E2EE key id, a fixed function of the account.
func e2eeKeyID(u auth.User) string { return "aek_" + strings.ReplaceAll(u.ID, "_", "")[:20] }

// uuidFor returns a version-4 UUID shaped value derived from id, as tunnels require.
func uuidFor(id string) string {
	h := hex.EncodeToString(digest(id))
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

func (f *fixture) virtualAddress() string {
	n := time.Now().UnixNano()
	return fmt.Sprintf("100.127.%d.%d", (n/256)%250+1, n%250+1)
}

// seedTeam creates a team owned by owner with the given extra active members.
func (f *fixture) seedTeam(owner auth.User, name string, members ...string) string {
	f.t.Helper()
	team := f.id("team")
	f.exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,$3)`, team, owner.ID, name)
	f.exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner.ID)
	for _, m := range members {
		f.exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, m)
	}
	return team
}

func (f *fixture) jobs(userID string) map[string]int {
	f.t.Helper()
	out := map[string]int{}
	rows, err := f.store.SQL().QueryContext(f.t.Context(), `SELECT kind, state FROM account_deletion_jobs WHERE user_id=$1`, userID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, state string
		if err := rows.Scan(&kind, &state); err != nil {
			f.t.Fatal(err)
		}
		out[kind+":"+state]++
	}
	return out
}

func TestAccountDeletionErasesPersonalData(t *testing.T) {
	f := newFixture(t)
	u := f.user("erase")
	other := f.user("erase_other")
	machine := f.seedPersonal(u)
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'other','linux','amd64','/workspace','online','occupied',true,1)`, "m2_"+u.ID, u.ID)
	ownedAlone := f.seedTeam(u, "Solo team")
	inOther := f.seedTeam(other, "Other team", u.ID)
	if err := f.svc.Delete(t.Context(), u, time.Now(), u.PrimaryEmail); err != nil {
		t.Fatal(err)
	}
	checks := map[string]struct {
		query string
		want  int
	}{
		"browser session revoked":          {`SELECT count(*) FROM sessions WHERE user_id=$1 AND revoked_at IS NULL`, 0},
		"CLI session revoked":              {`SELECT count(*) FROM cli_client_sessions WHERE user_id=$1 AND state='active'`, 0},
		"CLI refresh token revoked":        {`SELECT count(*) FROM cli_refresh_tokens rt JOIN cli_client_sessions cs ON cs.id=rt.cli_client_session_id WHERE cs.user_id=$1 AND rt.state='active'`, 0},
		"CLI access token revoked":         {`SELECT count(*) FROM cli_access_tokens at JOIN cli_client_sessions cs ON cs.id=at.cli_client_session_id WHERE cs.user_id=$1 AND at.revoked_at IS NULL`, 0},
		"API token revoked":                {`SELECT count(*) FROM api_tokens WHERE account_id=$1 AND revoked_at IS NULL`, 0},
		"machines deleted":                 {`SELECT count(*) FROM user_machines WHERE user_id=$1 AND deleted_at IS NULL`, 0},
		"favorites removed":                {`SELECT count(*) FROM user_favorites WHERE user_id=$1`, 0},
		"identity links removed":           {`SELECT count(*) FROM user_identities WHERE user_id=$1`, 0},
		"E2EE keys removed":                {`SELECT count(*) FROM account_e2ee_keys WHERE user_id=$1`, 0},
		"peer identities removed":          {`SELECT count(*) FROM peer_network_identities WHERE user_id=$1`, 0},
		"personal ENV removed":             {`SELECT count(*) FROM environment_vault_scopes WHERE owner_kind='personal' AND owner_id=$1`, 0},
		"previews stopped":                 {`SELECT count(*) FROM preview_leases WHERE account_id=$1 AND terminal_state='active'`, 0},
		"tunnels deleted":                  {`SELECT count(*) FROM tunnels WHERE account_id=$1 AND deleted_at IS NULL`, 0},
		"single-member team deleted":       {`SELECT count(*) FROM teams WHERE team_id=$1 AND deleted_at IS NULL`, 0},
		"membership of other team removed": {`SELECT count(*) FROM team_members WHERE team_id=$1 AND account_id=$2 AND active`, 0},
		"subscription kept for invoices":   {`SELECT count(*) FROM billing_workspaces WHERE user_id=$1 AND provider_subscription_id IS NOT NULL`, 1},
		"audit actor cleared":              {`SELECT count(*) FROM audit_events WHERE actor_user_id=$1 OR account_id=$1`, 0},
	}
	for name, c := range checks {
		var n int
		if name == "membership of other team removed" {
			n = f.count(c.query, inOther, u.ID)
		} else if name == "single-member team deleted" {
			n = f.count(c.query, ownedAlone)
		} else {
			n = f.count(c.query, u.ID)
		}
		if n != c.want {
			t.Errorf("%s: %d rows, want %d", name, n, c.want)
		}
	}
	if got := f.count(`SELECT count(*) FROM audit_events WHERE actor_type='deleted_user' AND actor_email IS NULL AND actor_user_id IS NULL`); got == 0 {
		t.Error("the anonymized audit event is missing")
	}
	var status, email string
	if err := f.store.SQL().QueryRowContext(t.Context(), `SELECT status, primary_email FROM users WHERE id=$1`, u.ID).Scan(&status, &email); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" || strings.Contains(email, "@") {
		t.Errorf("tombstone status=%q email=%q", status, email)
	}
	want := map[string]int{"workos_user:queued": 1, "stripe_subscription:queued": 1, "confirmation_mail:queued": 1}
	got := f.jobs(u.ID)
	for k, n := range want {
		if got[k] != n {
			t.Errorf("job %s: %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	_ = machine
}

func TestAccountDeletionRefusals(t *testing.T) {
	f := newFixture(t)
	u := f.user("refuse")
	f.seedPersonal(u)
	stale := time.Now().Add(-ReauthenticationWindow - time.Minute)
	if err := f.svc.Delete(t.Context(), u, stale, u.PrimaryEmail); !errors.Is(err, ErrReauthenticationRequired) {
		t.Fatalf("stale sign-in: %v", err)
	}
	if err := f.svc.Delete(t.Context(), u, time.Now(), "someone@example.test"); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("wrong email: %v", err)
	}
	member := f.user("refuse_member")
	shared := f.seedTeam(u, "Shared team", member.ID)
	err := f.svc.Delete(t.Context(), u, time.Now(), u.PrimaryEmail)
	var owned *OwnedTeamsError
	if !errors.As(err, &owned) || len(owned.Teams) != 1 || owned.Teams[0].Slug != shared || owned.Teams[0].Members != 2 {
		t.Fatalf("owned team with members: %v", err)
	}
	if f.count(`SELECT count(*) FROM users WHERE id=$1 AND status='active'`, u.ID) != 1 || f.count(`SELECT count(*) FROM sessions WHERE user_id=$1 AND revoked_at IS NULL`, u.ID) != 1 {
		t.Fatal("a refused deletion changed the account")
	}
}

func TestAccountDeletionAllowsSignUpAgain(t *testing.T) {
	f := newFixture(t)
	u := f.user("again")
	f.seedPersonal(u)
	if err := f.svc.Delete(t.Context(), u, time.Now(), u.PrimaryEmail); err != nil {
		t.Fatal(err)
	}
	verifier := fixedVerifier{subject: "subj_new_" + u.ID, email: u.PrimaryEmail}
	writer := f.writer
	signIn := auth.NewService(f.store, writer, verifier, []string{"test-accountdeletion-key"}, false)
	again, _, err := signIn.VerifyCallback(t.Context(), auth.CallbackInput{Code: "code", RedirectURI: "https://console.example.test/callback", State: "state"})
	if err != nil {
		t.Fatalf("sign-up after deletion: %v", err)
	}
	if again.ID == u.ID || again.Status != "active" || again.PrimaryEmail != u.PrimaryEmail {
		t.Fatalf("new account = %+v, deleted account %s", again, u.ID)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = f.store.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE user_id=$1`, again.ID)
	})
}

// fixedVerifier answers every sign-in with one WorkOS profile.
type fixedVerifier struct {
	subject, email string
}

func (v fixedVerifier) VerifyCallback(context.Context, auth.CallbackInput) (auth.WorkOSProfile, error) {
	return auth.WorkOSProfile{Subject: v.subject, Email: v.email, EmailVerified: true, DisplayName: "Again"}, nil
}
