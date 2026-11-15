package teaminbox

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teambucket"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	bucket "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

type storedBucketCall struct {
	key, digest string
	size        int64
	ttl         time.Duration
}

// This fake owns only the bucket boundary: the service still owns all SQL,
// authorization, lifecycle transitions, immutable metadata and object keys.
type storedTestBucket struct {
	mu         sync.Mutex
	enabled    bool
	missing    bool
	deleteErr  error
	puts, gets []storedBucketCall
	objects    map[string]bool
	deleted    []string
}

func (b *storedTestBucket) StoreFor(context.Context, *db.Tx, string) (teambucket.ObjectStore, error) {
	if b.missing {
		return nil, teambucket.ErrNotFound
	}
	return b, nil
}
func (b *storedTestBucket) Config() bucket.Config {
	return bucket.Config{Prefix: "stored-tests", UseFiles: b.enabled}
}
func (b *storedTestBucket) PresignPut(_ context.Context, key string, size int64, digest string, ttl time.Duration) (bucket.Authority, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts = append(b.puts, storedBucketCall{key: key, size: size, digest: digest, ttl: ttl})
	return bucket.Authority{URL: "https://bucket.example.test/" + key}, nil
}
func (b *storedTestBucket) PresignGet(_ context.Context, key string, ttl time.Duration) (bucket.Authority, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets = append(b.gets, storedBucketCall{key: key, ttl: ttl})
	return bucket.Authority{URL: "https://bucket.example.test/" + key}, nil
}
func (b *storedTestBucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	delete(b.objects, key)
	b.deleted = append(b.deleted, key)
	return nil
}

type storedFixture struct {
	t                              *testing.T
	ctx                            context.Context
	db                             *db.DB
	s                              *Service
	b                              *storedTestBucket
	sender, recipient, team        string
	source, destination, alternate string
	writer                         []byte
	now                            time.Time
}

func newStoredFixture(t *testing.T) *storedFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	f := &storedFixture{t: t, ctx: ctx, db: inboxTestStore(t), now: time.Now().UTC().Truncate(time.Second),
		sender: "stored_sender_" + suffix, recipient: "stored_recipient_" + suffix, team: "stored_team_" + suffix,
		source: "stored_source_" + suffix, destination: "stored_dest_" + suffix, alternate: "stored_alt_" + suffix,
		b: &storedTestBucket{enabled: true, objects: map[string]bool{}}}
	for _, account := range []string{f.sender, f.recipient} {
		f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.writer = public
	for _, m := range []struct{ id, account string }{{f.source, f.sender}, {f.destination, f.recipient}, {f.alternate, f.recipient}} {
		f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
 VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['file_receive'],ARRAY['file_receive'])`, m.id, m.account, "env_"+m.id)
		if m.account == f.recipient {
			host, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			f.exec(`INSERT INTO paperboat.environment_vault_hosts(machine_id,account_id,installation_generation,host_key_generation,host_public) VALUES($1,$2,1,1,$3)`, m.id, m.account, host.PublicKey().Bytes())
		}
	}
	f.exec(`UPDATE paperboat.user_machines SET public_identity_key=$2 WHERE id=$1`, f.source, base64.RawURLEncoding.EncodeToString(public))
	authority := teams.NewService(f.db)
	current, err := authority.Create(ctx, f.recipient, teams.CreateRequest{OperationID: "create_" + suffix, TeamID: f.team})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES($1,$2,'active',2,now()-interval '1 day',now()+interval '30 days')`, "team:"+f.team, f.team)
	current, err = authority.Get(ctx, f.recipient, f.team)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := authority.Invite(ctx, f.recipient, f.team, teams.InviteRequest{OperationID: "invite_" + suffix, ExpectedGeneration: current.Generation, AccountID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	current, err = authority.Accept(ctx, f.sender, invite.InvitationID, teams.AcceptRequest{OperationID: "accept_" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	current, err = authority.Machine(ctx, f.recipient, f.team, teams.MachineRequest{OperationID: "share_" + suffix, ExpectedGeneration: current.Generation, MachineID: f.destination, Action: "share"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authority.GrantMachine(ctx, f.recipient, f.team, teams.MachineGrantRequest{OperationID: "grant_" + suffix, ExpectedGeneration: current.Generation, MachineID: f.destination, Audience: "selected_member", AccountID: f.sender, Capabilities: []string{"files"}, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = New(f.db, config.Default().Access.FileTransfer)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetBuckets(f.b)
	f.s.now = func() time.Time { return f.now }
	f.exec(`UPDATE paperboat.user_machines SET state='offline',online=false WHERE id=$1`, f.destination)
	return f
}

func (f *storedFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.SQL().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *storedFixture) create() (Request, error) {
	id := uuid.NewString()
	return f.s.Create(f.ctx, RequestInput{RequestID: "request_" + id, OperationID: "op_" + id,
		SenderAccount: f.sender, SourceMachineID: f.source, DestinationMachineID: f.destination,
		BatchID: "fb_" + id, Files: []File{{Basename: "proof.txt", Size: 5, SHA256: strings.Repeat("ab", 32)}},
		ExpiresAt: f.now.Add(time.Hour)})
}
func (f *storedFixture) metadata() wire.Metadata {
	return wire.Metadata{WriterPublic: bytes.Clone(f.writer),
		Parts:      []wire.Part{{Index: 0, File: 0, Offset: 0, Bytes: 33, SHA256: strings.Repeat("cd", 32)}},
		SealedKeys: map[string][]byte{f.destination: bytes.Repeat([]byte{1}, 64), f.alternate: bytes.Repeat([]byte{2}, 64)}}
}
func (f *storedFixture) uploaded() Request {
	f.t.Helper()
	r, err := f.create()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.PutStored(f.ctx, f.sender, r.RequestID, f.metadata()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.StoredUploadURLs(f.ctx, f.sender, r.RequestID, wire.Range{First: 0, Count: 1}); err != nil {
		f.t.Fatal(err)
	}
	key := "stored-tests/v1/files/" + r.RequestID + "/000000.part"
	f.b.objects[key] = true
	if err := f.s.StoredUploaded(f.ctx, f.sender, r.RequestID); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestPostgresStoredCreateOfflineAndFrozenManifest(t *testing.T) {
	f := newStoredFixture(t)
	f.s.SetBuckets(nil)
	if _, err := f.create(); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline without a bucket: %v", err)
	}
	f.s.SetBuckets(f.b)
	f.b.enabled = false
	if _, err := f.create(); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline with files disabled: %v", err)
	}
	f.b.enabled = true
	if _, err := f.s.SetPolicy(f.ctx, f.recipient, Policy{Acceptance: "automatic"}, 1); err != nil {
		t.Fatal(err)
	}
	r, err := f.create()
	if err != nil || !r.Stored || r.Status != "pending" || !r.ExpiresAt.Equal(f.now.Add(30*24*time.Hour)) {
		t.Fatalf("stored request must remain accept-first for 30 days: stored=%v status=%s expiry=%s err=%v", r.Stored, r.Status, r.ExpiresAt, err)
	}
	recipients, err := f.s.StoredRecipients(f.ctx, f.sender, r.RequestID)
	if err != nil || len(recipients) != 2 || recipients[0].MachineID == recipients[1].MachineID || len(recipients[0].HostPublic) != 32 {
		t.Fatal("recipient's enrolled ENV hosts were not returned", err)
	}
	missing := f.metadata()
	delete(missing.SealedKeys, f.alternate)
	if err := f.s.PutStored(f.ctx, f.sender, r.RequestID, missing); !errors.Is(err, ErrInvalid) {
		t.Fatalf("metadata without every recipient key accepted: %v", err)
	}
	m := f.metadata()
	if err := f.s.PutStored(f.ctx, f.sender, r.RequestID, m); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutStored(f.ctx, f.sender, r.RequestID, m); err != nil {
		t.Fatal("exact metadata retry failed", err)
	}
	changed := f.metadata()
	changed.Parts[0].SHA256 = strings.Repeat("ef", 32)
	if err := f.s.PutStored(f.ctx, f.sender, r.RequestID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("frozen metadata replaced: %v", err)
	}
	if _, err := f.s.StoredUploadURLs(f.ctx, f.recipient, r.RequestID, wire.Range{Count: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recipient could upload sender's ciphertext: %v", err)
	}
	urls, err := f.s.StoredUploadURLs(f.ctx, f.sender, r.RequestID, wire.Range{Count: 1})
	if err != nil || len(urls.URLs) != 1 || len(f.b.puts) != 1 {
		t.Fatal("upload authority missing", err)
	}
	put := f.b.puts[0]
	if put.key != "stored-tests/v1/files/"+r.RequestID+"/000000.part" || put.size != 33 || put.digest != strings.Repeat("cd", 32) || put.ttl != 10*time.Minute {
		t.Fatal("PUT was not bound to frozen ciphertext size, digest, namespace and lifetime")
	}
	if _, err := f.s.StoredUploadURLs(f.ctx, f.sender, r.RequestID, wire.Range{Count: bucket.MaxSegmentsPerBatch + 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded upload URL batch accepted: %v", err)
	}
}

func TestPostgresStoredApprovalClaimsOneMachineAndDeletesOnReceipt(t *testing.T) {
	f := newStoredFixture(t)
	r := f.uploaded()
	page, err := f.s.StoredDeliverable(f.ctx, f.destination)
	if err != nil || len(page.Transfers) != 0 {
		t.Fatal("pending ciphertext was deliverable before acceptance", err)
	}
	if _, err := f.s.StoredDownloadURLs(f.ctx, f.destination, r.RequestID, wire.Range{Count: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download before approval: %v", err)
	}
	if _, err := f.s.Decide(f.ctx, f.recipient, r.RequestID, "approved", r.DecisionGeneration); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stored approval without a receiving machine: %v", err)
	}
	if _, err := f.s.Decide(f.ctx, f.recipient, r.RequestID, "approved", r.DecisionGeneration, f.source); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stored approval into another account's machine: %v", err)
	}
	if _, err := f.s.Decide(f.ctx, f.recipient, r.RequestID, "approved", r.DecisionGeneration, f.alternate); err != nil {
		t.Fatal(err)
	}
	page, err = f.s.StoredDeliverable(f.ctx, f.alternate)
	if err != nil || len(page.Transfers) != 1 || page.Transfers[0].RequestID != r.RequestID || len(page.Transfers[0].Metadata.SealedKeys) != 1 || len(page.Transfers[0].Metadata.SealedKeys[f.alternate]) == 0 {
		t.Fatal("approved transfer did not address the alternate host alone", err)
	}
	page, err = f.s.StoredDeliverable(f.ctx, f.source)
	if err != nil || len(page.Transfers) != 0 {
		t.Fatal("sender machine could receive recipient's file", err)
	}
	type claim struct {
		machine string
		err     error
	}
	claims := make(chan claim, 2)
	for _, machine := range []string{f.destination, f.alternate} {
		go func() {
			_, err := f.s.StoredDownloadURLs(f.ctx, machine, r.RequestID, wire.Range{Count: 1})
			claims <- claim{machine, err}
		}()
	}
	winner, denied := "", 0
	for range 2 {
		c := <-claims
		if c.err == nil {
			winner = c.machine
		} else if errors.Is(c.err, ErrNotFound) {
			denied++
		} else {
			t.Fatal(c.err)
		}
	}
	if winner != f.alternate || denied != 1 {
		t.Fatal("a concurrent host could steal delivery from the machine that accepted")
	}
	if _, err := f.s.StoredDownloadURLs(f.ctx, winner, r.RequestID, wire.Range{Count: 1}); err != nil {
		t.Fatal("claimed host could not resume", err)
	}
	if err := f.s.StoredReceived(f.ctx, winner, r.RequestID, strings.Repeat("ff", 32)); !errors.Is(err, ErrNotFound) || len(f.b.objects) != 1 {
		t.Fatal("wrong manifest receipt deleted objects", err)
	}
	if err := f.s.StoredReceived(f.ctx, winner, r.RequestID, r.ManifestDigest); err != nil || len(f.b.objects) != 0 {
		t.Fatal("verified receipt did not delete bucket objects", err)
	}
	if err := f.s.StoredReceived(f.ctx, winner, r.RequestID, r.ManifestDigest); err != nil {
		t.Fatal("receipt retry was not idempotent", err)
	}
	f.now = f.now.Add(11 * time.Minute)
	if err := f.s.PruneStored(f.ctx); err != nil {
		t.Fatal(err)
	}
	var deleted sql.NullTime
	if err := f.db.SQL().QueryRowContext(f.ctx, `SELECT stored_deleted_at FROM paperboat.team_inbox_requests WHERE request_id=$1`, r.RequestID).Scan(&deleted); err != nil || !deleted.Valid {
		t.Fatal("receipt cleanup did not finish after PUT authority expiry", err)
	}
	if err := f.s.StoredReceived(f.ctx, winner, r.RequestID, r.ManifestDigest); err != nil {
		t.Fatal("receipt retry after completed cleanup was not idempotent", err)
	}
}

func TestPostgresStoredDeclineExpiryAndRevocationCleanLateUploads(t *testing.T) {
	for _, reason := range []string{"decline", "expiry", "revocation"} {
		t.Run(reason, func(t *testing.T) {
			f := newStoredFixture(t)
			r := f.uploaded()
			switch reason {
			case "decline":
				if _, err := f.s.Decide(f.ctx, f.recipient, r.RequestID, "declined", r.DecisionGeneration); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				f.exec(`UPDATE paperboat.team_inbox_requests SET expires_at=$2 WHERE request_id=$1`, r.RequestID, f.now.Add(-time.Second))
			case "revocation":
				f.exec(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, f.team, f.sender)
			}
			if err := f.s.PruneStored(f.ctx); err != nil || len(f.b.objects) != 0 {
				t.Fatal("terminal request left ciphertext objects", err)
			}
			var deleted sql.NullTime
			if err := f.db.SQL().QueryRowContext(f.ctx, `SELECT stored_deleted_at FROM paperboat.team_inbox_requests WHERE request_id=$1`, r.RequestID).Scan(&deleted); err != nil || deleted.Valid {
				t.Fatal("cleanup forgot outstanding PUT authority", err)
			}
			// A sender can still use a previously issued PUT until its expiry.
			key := "stored-tests/v1/files/" + r.RequestID + "/000000.part"
			f.b.objects[key] = true
			f.now = f.now.Add(11 * time.Minute)
			f.b.deleteErr = errors.New("bucket temporarily unavailable")
			if err := f.s.PruneStored(f.ctx); err == nil || len(f.b.objects) != 1 {
				t.Fatal("failed deletion was forgotten")
			}
			f.b.deleteErr = nil
			if err := f.s.PruneStored(f.ctx); err != nil || len(f.b.objects) != 0 {
				t.Fatal("late ciphertext PUT was not cleaned on retry", err)
			}
			if err := f.db.SQL().QueryRowContext(f.ctx, `SELECT stored_deleted_at FROM paperboat.team_inbox_requests WHERE request_id=$1`, r.RequestID).Scan(&deleted); err != nil || !deleted.Valid {
				t.Fatal("final cleanup was not recorded", err)
			}
			page, err := f.s.StoredDeliverable(f.ctx, f.destination)
			if err != nil || len(page.Transfers) != 0 {
				t.Fatal("terminal request remained deliverable", err)
			}
		})
	}
}
