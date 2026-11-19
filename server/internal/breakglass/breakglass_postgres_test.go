package breakglass

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/apitokens"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"golang.org/x/crypto/ssh"
)

const (
	oldRing = "old-encryption-key-0123456789-0123456789-0123"
	newKey  = "new-encryption-key-0123456789-0123456789-0123"
	// The ring a recovered server is deployed with: the new key first, the compromised one kept to open old data.
	ring = newKey + "," + oldRing
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

type fakeMint struct {
	active    string
	published []string
}

func (m fakeMint) KeyIDs() (string, []string) { return m.active, m.published }

type fakeTOTP struct {
	mu    sync.Mutex
	spent []string
}

func (f *fakeTOTP) CheckTOTP(_ context.Context, operatorID, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code != "123456" {
		return operatorauth.ErrMFAInvalid
	}
	f.spent = append(f.spent, operatorID)
	return nil
}

type fakeFloor struct {
	policy clientupdate.Policy
	puts   []clientupdate.Input
}

func (f *fakeFloor) Get(context.Context) (clientupdate.Policy, error) { return f.policy, nil }
func (f *fakeFloor) Put(_ context.Context, _, _, _ string, in clientupdate.Input) (clientupdate.Policy, error) {
	f.puts = append(f.puts, in)
	f.policy = clientupdate.Policy{Revision: f.policy.Revision + 1, MinimumVersion: in.MinimumVersion}
	return f.policy, nil
}

// world is one database with the services a run drives, a controllable clock and the fakes for what is configuration.
type world struct {
	t     *testing.T
	store *db.DB
	ssh   *managedssh.Service
	svc   *Service
	cfg   Config
	totp  *fakeTOTP
	floor *fakeFloor
	clock time.Time
	sfx   string
	// fetching lists the machines that fetch their CAs after every step, a second apart, as hosts do.
	fetching []machine
	jumped   bool
}

func newWorld(t *testing.T) *world {
	t.Helper()
	store := testDB(t)
	w := &world{t: t, store: store, totp: &fakeTOTP{}, floor: &fakeFloor{}, clock: time.Now().UTC().Truncate(time.Second), sfx: strings.ReplaceAll(uuid.NewString()[:8], "-", "")}
	repository, err := managedssh.NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	if w.ssh, err = managedssh.NewService(repository, managedssh.CertificateConfig{EncryptionKey: oldRing, Validity: time.Hour}); err != nil {
		t.Fatal(err)
	}
	w.cfg = Config{
		Store: store, SSH: w.ssh, Mint: fakeMint{active: "mint-new", published: []string{"mint-new", "mint-old"}}, Floor: w.floor, TOTP: w.totp,
		EncryptionKey: ring, SessionKeys: []string{"session-key-new-0123456789-0123456789", "session-key-old-0123456789-0123456789"},
		PickupThreshold: 1, PickupTimeout: time.Hour, MintDrain: 30 * time.Minute, OnlineWindow: time.Hour, Batch: 2, Now: func() time.Time { return w.clock },
	}
	w.rebuild()
	// One run at a time per database: a run another test left behind must not block this one.
	end := func() {
		w.exec(`UPDATE paperboat.breakglass_runs SET state = 'cancelled', finished_at = now() WHERE state = 'running'`)
	}
	end()
	w.exec(`DELETE FROM paperboat.managed_ssh_ca_fetches`) // machines of earlier tests do not fetch any more
	w.exec(`DELETE FROM paperboat.mint_withdrawn_keys`)
	// A run publishes the CA of every account in the database: accounts other tests (or earlier iterations) left would be counted.
	w.exec(`DELETE FROM paperboat.managed_ssh_user_cas`)
	t.Cleanup(end)
	return w
}

// rebuild makes a new Service on the same database: a restarted server.
func (w *world) rebuild() {
	w.t.Helper()
	svc, err := New(w.cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	w.svc = svc
}

func (w *world) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.store.SQL().ExecContext(context.Background(), query, args...); err != nil {
		w.t.Fatalf("%v\n%s", err, query)
	}
}

func (w *world) operator(name string) string {
	id := "opr_" + name + w.sfx
	w.exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','admin')`, id)
	return id
}

type machine struct{ user, id, session string }

// machine adds an enrolled machine with a registered SSH target and a CLI session of its owner.
func (w *world) machine(name string) machine {
	w.t.Helper()
	m := machine{user: "bg_user_" + name + w.sfx, id: "bg_machine_" + name + w.sfx, session: "bg_cli_" + name + w.sfx}
	w.exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$1,$1||'@example.test','active')`, m.user)
	w.exec(`INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$1,'t','desktop','test',ARRAY['account:read'],'active',$3,$3)`, m.session, m.user, w.clock.Add(-time.Minute))
	w.exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, m.id, m.user, "env_"+m.id)
	if _, err := w.ssh.RegisterTarget(context.Background(), managedssh.RegisterTargetRequest{OperationID: "operation_target_" + m.id, ActorUserID: m.user, UserMachineID: m.id, MachineGeneration: 1, OSUser: "deploy", TargetPort: 22, Now: w.clock}); err != nil {
		w.t.Fatal(err)
	}
	return m
}

// fetch is the machine fetching its trusted CAs, as its host does every few minutes.
func (w *world) fetch(m machine) managedssh.TrustedUserCASet {
	w.t.Helper()
	set, err := w.ssh.TrustedUserCAs(context.Background(), managedssh.UserCAsRequest{ActorUserID: m.user, UserMachineID: m.id, MachineGeneration: 1, Now: w.clock})
	if err != nil {
		w.t.Fatal(err)
	}
	return set
}

// issue is the account's CLI session getting a certificate for the machine from the server's signing CA.
func (w *world) issue(m machine, client ssh.PublicKey) *ssh.Certificate {
	w.t.Helper()
	out, err := w.ssh.IssueCertificate(context.Background(), managedssh.IssueCertificateRequest{UserID: m.user, CLIClientSessionID: m.session, UserMachineID: m.id, MachineGeneration: 1, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(client))), Now: w.clock})
	if err != nil {
		w.t.Fatal(err)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(out.Certificate))
	if err != nil {
		w.t.Fatal(err)
	}
	return parsed.(*ssh.Certificate)
}

// accepts is the machine's sshd check: a certificate signed by a CA the machine trusts, for its principal, from the
// loopback bridge.
func (w *world) accepts(set managedssh.TrustedUserCASet, certificate *ssh.Certificate) bool {
	checker := ssh.CertChecker{
		Clock: func() time.Time { return w.clock },
		IsUserAuthority: func(authority ssh.PublicKey) bool {
			for _, key := range set.Keys {
				parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
				if err == nil && bytes.Equal(parsed.Marshal(), authority.Marshal()) {
					return true
				}
			}
			return false
		},
		SupportedCriticalOptions: []string{"source-address"},
	}
	_, err := checker.Authenticate(fakeConn{user: set.Principal}, certificate)
	return err == nil
}

type fakeConn struct {
	ssh.ConnMetadata
	user string
}

func (c fakeConn) User() string       { return c.user }
func (fakeConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40000} }

func (w *world) start(operator string, floor string) Run {
	w.t.Helper()
	run, err := w.svc.Start(context.Background(), StartInput{OperatorID: operator, Phrase: Phrase, Reason: "server breach drill", TOTPCode: "123456", FloorVersion: floor})
	if err != nil {
		w.t.Fatal(err)
	}
	return run
}

// step runs the worker once and returns the run.
func (w *world) step(id string) Run {
	w.t.Helper()
	if err := w.svc.Step(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	w.clock = w.clock.Add(time.Second)
	for _, m := range w.fetching {
		w.fetch(m)
	}
	run, err := w.svc.Get(context.Background(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	// Retire removes the CAs at once but keeps the old mint key until machine credentials signed by it have
	// expired: it is still published here, and the clock then jumps past the drain.
	if retire := stageOf(run, StageRetire); retire.Detail["ca_pickup_done"] == true && retire.State != stageDone && !w.jumped {
		if slices.Contains(w.mintWithdrawn(), "mint-old") {
			w.t.Fatal("the old mint key was withdrawn before machines could renew their credentials")
		}
		w.jumped = true
		w.clock = w.clock.Add(w.cfg.MintDrain)
	}
	return run
}

func stageOf(run Run, stage Stage) StageState { return run.Stages[indexOf(stage)] }

// until steps until the stage is done (or the run is), failing after a bound.
func (w *world) until(id string, stage Stage) Run {
	w.t.Helper()
	for range 200 {
		run := w.step(id)
		if stageOf(run, stage).State == stageDone || run.State != "running" {
			return run
		}
		if run.LastError != "" {
			w.t.Fatalf("stage %s failed: %s", stage, run.LastError)
		}
	}
	run, _ := w.svc.Get(context.Background(), id)
	w.t.Fatalf("stage %s did not finish: %+v", stage, stageOf(run, stage))
	return Run{}
}

func clientKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The machine keeps accepting certificates through the whole overlap with no change on the machine, and refuses
// the old key once the run retired it. Both keys are accepted from publish until retire.
func TestMachineTrustsOldAndNewKeysDuringTheOverlapAndRefusesTheOldAfterRetire(t *testing.T) {
	w := newWorld(t)
	operator := w.operator("overlap")
	m, other := w.machine("a"), w.machine("b")
	clientPublic := clientKey(t)
	before := w.issue(m, clientPublic)
	if set := w.fetch(m); len(set.Keys) != 1 || !w.accepts(set, before) {
		t.Fatalf("setup: the machine must trust the one CA that signed %+v", set)
	}
	w.fetch(other)
	w.clock = w.clock.Add(time.Minute)

	run := w.start(operator, "")
	w.until(run.ID, StagePublish)
	// Each CA change wakes the account's machines (machine_access topic) so they fetch now, not at their next timer.
	var woken int
	if err := w.store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='machine_access' AND updated_at >= $2`, m.id, w.clock.Add(-time.Hour)).Scan(&woken); err != nil || woken != 1 {
		t.Fatalf("publish must wake the account's machines: %d %v", woken, err)
	}
	// (a) published: the machine's next fetch holds the old and the pending CA, and still accepts the old one.
	w.clock = w.clock.Add(time.Minute)
	set := w.fetch(m)
	if len(set.Keys) != 2 || !w.accepts(set, before) {
		t.Fatalf("after publish the machine must trust old and next CAs: %d keys, old accepted=%v", len(set.Keys), w.accepts(set, before))
	}
	// (b) pickup waits for the other machine; once it fetched too, the stage ends with both counted.
	if r := w.step(run.ID); stageOf(r, StagePickup).State == stageDone {
		t.Fatal("pickup finished before the second online machine fetched the new CAs")
	}
	w.fetch(other)
	w.fetching = []machine{m, other}
	run = w.until(run.ID, StagePickup)
	if d := stageOf(run, StagePickup).Detail; detailInt(d, "expected") != 2 || detailInt(d, "picked_up") != 2 {
		t.Fatalf("pickup detail = %v", d)
	}
	// (c) switch: certificates are now signed by the next CA; the machine accepts them and the old ones.
	w.until(run.ID, StageSwitch)
	after := w.issue(m, clientPublic)
	set = w.fetch(m)
	if !w.accepts(set, after) || !w.accepts(set, before) {
		t.Fatalf("during the overlap the machine must accept certificates of the old and of the new CA")
	}
	if after.SignatureKey.Type() != "ssh-ed25519" || bytes.Equal(after.SignatureKey.Marshal(), before.SignatureKey.Marshal()) {
		t.Fatal("the switch must sign with a different CA")
	}
	// (e) retire: once the machines fetched the set again the old CA is gone and refused.
	run = w.until(run.ID, StageRetire)
	set = w.fetch(m)
	if len(set.Keys) != 1 || w.accepts(set, before) || !w.accepts(set, after) {
		t.Fatalf("after retire the machine must trust only the new CA: keys=%d old accepted=%v new accepted=%v", len(set.Keys), w.accepts(set, before), w.accepts(set, after))
	}
	if r := w.step(run.ID); r.State != "done" {
		t.Fatalf("run state = %s", r.State)
	}
}

// (d) After the revoke stage no session or token of any account works, the acting operator's session survives,
// and a session created after the stage began is not touched.
func TestRevokeStageRefusesEverySessionAndTokenExceptTheActingOperators(t *testing.T) {
	w := newWorld(t)
	operator, other := w.operator("revoke"), w.operator("otherop")
	user := "bg_rev_user_" + w.sfx
	w.exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$1,$1||'@example.test','active')`, user)
	past, future := w.clock.Add(-time.Hour), w.clock.Add(time.Hour)
	w.exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,created_at) VALUES ($1,$2,$3,'csrf',$4,$5),($6,$2,$7,'csrf',$4,$8)`, "bg_s_old_"+w.sfx, user, "h1"+w.sfx, future, past, "bg_s_new_"+w.sfx, "h2"+w.sfx, future.Add(time.Hour))
	w.exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,'cli','t','cli','linux','{}','active',$3,$3)`, "bg_c_"+w.sfx, user, past)
	w.exec(`INSERT INTO paperboat.cli_access_tokens(token_hash,cli_client_session_id,expires_at,created_at) VALUES ($1,$2,$3,$4)`, "bg_at_"+w.sfx, "bg_c_"+w.sfx, future, past)
	w.exec(`INSERT INTO paperboat.cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES ($1,$2,'active',$3,$4)`, "bg_rt_"+w.sfx, "bg_c_"+w.sfx, future, past)
	secret := apitokens.SecretPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	hashed := sha256.Sum256([]byte(secret))
	w.exec(`INSERT INTO paperboat.api_tokens(id,account_id,name,secret_hash,created_at,expires_at) VALUES ($1,$2,'ci',$3,$4,$5)`, "bg_tok_"+w.sfx, user, hashed[:], past, future)
	w.exec(`INSERT INTO paperboat.recipes(id,token_id,account_id,name,source,source_format,canonical,auth_hash,created_at,expires_at) VALUES ($1,$1,$2,'r','x','toml','{}'::jsonb,sha256(convert_to($3,'UTF8')),$4,$5)`, "bg_rcp_"+w.sfx, user, "recipe"+w.sfx, past, future)
	// A breach exposes unused enrollment tokens and unredeemed install material: both are cancelled, a token minted
	// after the stage began is not, and an installed machine's pairing is left alone.
	pairing := func(id, state, installation string, created time.Time, bootstrap, install bool) {
		cols, vals := `id,state,installation_state,generation,expires_at,created_at,updated_at`, `$1,$2,$3,1,now()+interval '1 hour',$4,now()`
		args := []any{id, state, installation, created}
		if bootstrap {
			cols += `,bootstrap_owner_account,operation_id,idempotency_key,bootstrap_token_hash,bootstrap_token_ciphertext`
			vals += `,$5,'operation_'||$1,'idem_'||$1,sha256(convert_to($1,'UTF8')),'\x0102'::bytea`
			args = append(args, user)
		}
		if install {
			cols += `,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,public_identity_key,installation_config_ciphertext`
			vals += `,sha256(convert_to($1,'UTF8')),upper(substr(md5($1),1,8)),'a','linux','amd64','/workspace',repeat(md5($1),2),'\x0102'::bytea`
		}
		w.exec(`INSERT INTO paperboat.user_machine_pairings(`+cols+`) VALUES (`+vals+`)`, args...)
	}
	pairing("bg_pair_token_"+w.sfx, "awaiting_bootstrap", "awaiting_bootstrap", past, true, false)
	pairing("bg_pair_new_token_"+w.sfx, "awaiting_bootstrap", "awaiting_bootstrap", future.Add(time.Hour), true, false)
	pairing("bg_pair_install_"+w.sfx, "pending", "awaiting_approval", past, false, true)
	actingToken, otherToken := "acting-"+w.sfx, "other-"+w.sfx
	for _, s := range []struct{ operator, token string }{{operator, actingToken}, {other, otherToken}} {
		w.exec(`INSERT INTO paperboat.operator_sessions(token_hash,operator_id,csrf_hash,created_at,expires_at) VALUES (sha256(convert_to($1,'UTF8')),$2,sha256('c'),$3,$4)`, s.token, s.operator, past, future)
	}
	tokens := apitokens.New(w.store, audit.NewWriter(w.store), config.APITokens{}, ring)
	if _, err := tokens.Authenticate(context.Background(), secret); err != nil {
		t.Fatalf("setup: the API token must work before the run: %v", err)
	}
	keep := sha256.Sum256([]byte(actingToken))
	run, err := w.svc.Start(context.Background(), StartInput{OperatorID: operator, KeepSessionHash: keep[:], Phrase: Phrase, Reason: "server breach drill", TOTPCode: "123456"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Preview.Sessions["browser_sessions"] < 2 || run.Preview.OperatorSessions < 1 {
		t.Fatalf("the preview must count what will be revoked: %+v", run.Preview)
	}
	// A browser session created after the stage began (here: dated in the future) is not revoked.
	w.until(run.ID, StageRevoke)
	count := func(query string, args ...any) (n int) {
		t.Helper()
		if err := w.store.SQL().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM paperboat.sessions WHERE user_id=$1 AND revoked_at IS NULL`, user); n != 1 {
		t.Fatalf("only the browser session created after the stage began may survive, %d live", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.cli_client_sessions WHERE id=$1 AND state='active'`, "bg_c_"+w.sfx); n != 0 {
		t.Fatal("the CLI session was not revoked")
	}
	if n := count(`SELECT count(*) FROM paperboat.cli_access_tokens WHERE token_hash=$1 AND revoked_at IS NULL`, "bg_at_"+w.sfx) + count(`SELECT count(*) FROM paperboat.cli_refresh_tokens WHERE token_hash=$1 AND state<>'revoked'`, "bg_rt_"+w.sfx) + count(`SELECT count(*) FROM paperboat.recipes WHERE id=$1 AND revoked_at IS NULL`, "bg_rcp_"+w.sfx); n != 0 {
		t.Fatalf("%d CLI tokens or recipes still live", n)
	}
	pairingState := func(id string) (token, install bool, state, installation string) {
		t.Helper()
		if err := w.store.SQL().QueryRowContext(context.Background(), `SELECT bootstrap_token_hash IS NOT NULL, installation_config_ciphertext IS NOT NULL, state, installation_state FROM paperboat.user_machine_pairings WHERE id=$1`, id).Scan(&token, &install, &state, &installation); err != nil {
			t.Fatal(err)
		}
		return
	}
	if token, _, _, installation := pairingState("bg_pair_token_" + w.sfx); token || installation != "deleted" {
		t.Fatalf("the unused enrollment token must be cancelled: token=%v installation=%s", token, installation)
	}
	if _, install, state, _ := pairingState("bg_pair_install_" + w.sfx); install || state != "denied" {
		t.Fatalf("unredeemed install material must be cancelled: material=%v state=%s", install, state)
	}
	if token, _, _, installation := pairingState("bg_pair_new_token_" + w.sfx); !token || installation != "awaiting_bootstrap" {
		t.Fatalf("a token minted after the stage began must stay valid: token=%v installation=%s", token, installation)
	}
	if _, err := tokens.Authenticate(context.Background(), secret); !errors.Is(err, apitokens.ErrInvalid) {
		t.Fatalf("the API token must be refused after the revoke stage: %v", err)
	}
	live := func(token string) int {
		return count(`SELECT count(*) FROM paperboat.operator_sessions WHERE token_hash=sha256(convert_to($1,'UTF8')) AND revoked_at IS NULL`, token)
	}
	if live(actingToken) != 1 || live(otherToken) != 0 {
		t.Fatalf("the acting operator's session must survive and another operator's must not: acting=%d other=%d", live(actingToken), live(otherToken))
	}
}

// Every stored ciphertext moves to the newest key, a row nothing opens does not stop the run, and the server then
// works with the newest key alone (the old key is out of the ring).
func TestResealStageMovesEverySealedValueToTheNewestKeyAndReportsUnreadableRows(t *testing.T) {
	w := newWorld(t)
	operator := w.operator("reseal")
	m := w.machine("seal")
	w.fetch(m) // creates the account's CA, sealed under the old key
	totpOwner, broken := w.operator("totp"), w.operator("broken")
	sealed, err := secrets.Encrypt(oldRing, "JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO paperboat.operator_totp(operator_id,secret_cipher,confirmed_at) VALUES ($1,$2,now())`, totpOwner, sealed)
	// Everything stored before this build is in main's layout (nonce, sealed value: no key id): dropping the 12-byte
	// stamp of a value sealed by the old key gives exactly that layout.
	legacyOwner := w.operator("legacy")
	w.exec(`INSERT INTO paperboat.operator_totp(operator_id,secret_cipher,confirmed_at) VALUES ($1,$2,now())`, legacyOwner, sealed[12:])
	w.exec(`INSERT INTO paperboat.operator_totp(operator_id,secret_cipher,confirmed_at) VALUES ($1,$2,now())`, broken, bytes.Repeat([]byte{9}, 64))
	w.clock = w.clock.Add(time.Minute)
	w.fetching = []machine{m}
	run := w.start(operator, "")
	if run.Preview.Sealed["managed_ssh_user_cas.private_key_ciphertext"] < 1 || run.Preview.Sealed["operator_totp.secret_cipher"] < 2 {
		t.Fatalf("the preview must count rows under the old key: %v", run.Preview.Sealed)
	}
	run = w.until(run.ID, StageReseal)
	if n := detailInt(stageOf(run, StageReseal).Detail, "resealed"); n < 2 {
		t.Fatalf("resealed = %d", n)
	}
	if n := detailInt(stageOf(run, StageReseal).Detail, "undecryptable"); n < 1 {
		t.Fatalf("the unreadable row must be reported, got %d", n)
	}
	var cipher, legacy []byte
	if err := w.store.SQL().QueryRowContext(context.Background(), `SELECT secret_cipher FROM paperboat.operator_totp WHERE operator_id=$1`, totpOwner).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if err := w.store.SQL().QueryRowContext(context.Background(), `SELECT secret_cipher FROM paperboat.operator_totp WHERE operator_id=$1`, legacyOwner).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if got, err := secrets.Decrypt(newKey, legacy); secrets.NeedsReseal(ring, legacy) || err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("a value in main's layout must be re-sealed to the newest key: %q %v", got, err)
	}
	if secrets.NeedsReseal(ring, cipher) {
		t.Fatal("the TOTP secret is still under the old key")
	}
	if got, err := secrets.Decrypt(newKey, cipher); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("the new key alone must open the re-sealed secret: %q %v", got, err)
	}
	// A server deployed with only the new key still signs: the CA seed was re-sealed.
	repository, err := managedssh.NewSQLRepository(w.store, audit.NewWriter(w.store))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := managedssh.NewService(repository, managedssh.CertificateConfig{EncryptionKey: newKey, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// The run signed the old CLI session out; the account signs in again.
	fresh := "bg_cli_again_" + w.sfx
	w.exec(`INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES ($1,$2,$1,'t','desktop','test',ARRAY['account:read'],'active',now(),now())`, fresh, m.user)
	if _, err := recovered.IssueCertificate(context.Background(), managedssh.IssueCertificateRequest{UserID: m.user, CLIClientSessionID: fresh, UserMachineID: m.id, MachineGeneration: 1, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(clientKey(t)))), Now: w.clock}); err != nil {
		t.Fatalf("issuing with only the new encryption key: %v", err)
	}
}

// A server that restarts in the middle of a run continues it, and no stage does its work twice.
func TestRunResumesAfterARestartAndEveryStageIsAudited(t *testing.T) {
	w := newWorld(t)
	operator := w.operator("resume")
	m := w.machine("resume")
	w.fetch(m)
	run := w.start(operator, "2026.11.1.1")
	w.floor.policy = clientupdate.Policy{Revision: 3, MinimumVersion: "2026.1.1.1"}
	if _, err := w.svc.Start(context.Background(), StartInput{OperatorID: operator, Phrase: Phrase, Reason: "second run", TOTPCode: "123456"}); !errors.Is(err, ErrRunning) {
		t.Fatalf("a second run while one is running: %v", err)
	}
	w.step(run.ID)
	w.step(run.ID)
	w.rebuild() // the server crashed and came back
	w.fetching = []machine{m}
	for range 400 {
		run = w.step(run.ID)
		if run.State != "running" {
			break
		}
	}
	if run.State != "done" {
		t.Fatalf("run state = %s, error %q, stages %+v", run.State, run.LastError, run.Stages)
	}
	for _, stage := range run.Stages {
		if stage.State != stageDone || stage.StartedAt == nil || stage.FinishedAt == nil {
			t.Fatalf("stage %s: %+v", stage.Name, stage)
		}
	}
	if d := stageOf(run, StagePublish).Detail; detailInt(d, "published") != 1 {
		t.Fatalf("the account's CA must be published exactly once across the restart: %v", d)
	}
	if len(w.floor.puts) != 1 || w.floor.puts[0].MinimumVersion != "2026.11.1.1" || w.floor.puts[0].ExpectedRevision != 3 {
		t.Fatalf("the floor must be raised once with the revision read: %+v", w.floor.puts)
	}
	var started, finished int
	if err := w.store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FILTER (WHERE event_type='breakglass.stage_started'), count(*) FILTER (WHERE event_type='breakglass.stage_finished') FROM paperboat.audit_events WHERE resource_type='breakglass_run' AND resource_id=$1`, run.ID).Scan(&started, &finished); err != nil {
		t.Fatal(err)
	}
	if started != len(stageOrder)-0 || finished != len(stageOrder) {
		t.Fatalf("audited stages: started=%d finished=%d", started, finished)
	}
	if got := w.mintWithdrawn(); !slices.Contains(got, "mint-old") || slices.Contains(got, "mint-new") {
		t.Fatalf("only the old mint key may be withdrawn: %v", got)
	}
}

func (w *world) mintWithdrawn() []string {
	w.t.Helper()
	rows, err := w.store.SQL().QueryContext(context.Background(), `SELECT key_id FROM paperboat.mint_withdrawn_keys ORDER BY key_id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			w.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestStartIsGuardedByPhraseReasonCodeAndDeployedKeys(t *testing.T) {
	w := newWorld(t)
	operator := w.operator("guard")
	start := func(in StartInput) error {
		in.OperatorID = operator
		_, err := w.svc.Start(context.Background(), in)
		return err
	}
	good := StartInput{Phrase: Phrase, Reason: "server breach drill", TOTPCode: "123456"}
	bad := good
	bad.Phrase = "break glass please"
	if err := start(bad); !errors.Is(err, ErrPhrase) {
		t.Fatalf("wrong phrase: %v", err)
	}
	bad = good
	bad.Reason = "short"
	if err := start(bad); !errors.Is(err, ErrReason) {
		t.Fatalf("short reason: %v", err)
	}
	bad = good
	bad.TOTPCode = "000000"
	if err := start(bad); !errors.Is(err, operatorauth.ErrMFAInvalid) {
		t.Fatalf("wrong authenticator code: %v", err)
	}
	bad = good
	bad.FloorVersion = "1.2"
	if err := start(bad); !errors.Is(err, ErrFloor) {
		t.Fatalf("invalid floor: %v", err)
	}
	var runs int
	if err := w.store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM paperboat.breakglass_runs WHERE operator_id=$1`, operator).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("a refused start must leave no run: %d %v", runs, err)
	}
	// Keys not deployed yet: the run does not start and the error names what to deploy.
	w.cfg.Mint = fakeMint{active: "mint-old", published: []string{"mint-old"}}
	w.cfg.SessionKeys = w.cfg.SessionKeys[:1]
	w.cfg.EncryptionKey = oldRing
	w.rebuild()
	var notReady *NotReadyError
	err := start(good)
	if !errors.As(err, &notReady) || !strings.Contains(err.Error(), "mint signing keys") || !strings.Contains(err.Error(), "session keys") {
		t.Fatalf("undeployed keys: %v", err)
	}
	if len(w.totp.spent) != 0 {
		t.Fatalf("an authenticator code was spent for a start that was refused: %v", w.totp.spent)
	}
}

// The pickup wait ends at its timeout even when a machine never fetches (offline), and says so.
func TestPickupEndsAtTheTimeoutForMachinesThatStayOffline(t *testing.T) {
	w := newWorld(t)
	operator := w.operator("timeout")
	m, offline := w.machine("on"), w.machine("off")
	w.fetch(m)
	w.fetch(offline)
	w.clock = w.clock.Add(time.Minute)
	run := w.start(operator, "")
	w.until(run.ID, StagePublish)
	w.fetch(m)
	if r := w.step(run.ID); stageOf(r, StagePickup).State == stageDone {
		t.Fatal("pickup finished early")
	}
	w.clock = w.clock.Add(w.cfg.PickupTimeout + time.Second)
	run = w.until(run.ID, StagePickup)
	d := stageOf(run, StagePickup).Detail
	if d["timed_out"] != true || detailInt(d, "expected") != 2 || detailInt(d, "picked_up") != 1 {
		t.Fatalf("pickup detail = %v", d)
	}
	if err := w.svc.Cancel(context.Background(), run.ID, operator); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Cancel(context.Background(), run.ID, operator); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancelling twice: %v", err)
	}
}
