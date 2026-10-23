package releaseauthority

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestVerifyRequiresConfiguredThresholdAndCanonicalPayload(t *testing.T) {
	keys := make([]Key, 3)
	private := make([]ed25519.PrivateKey, 3)
	for i := range keys {
		public, secret, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = Key{ID: string(rune('a' + i)), Public: public}
		private[i] = secret
	}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	service := &Service{keys: map[string]ed25519.PublicKey{"a": keys[0].Public, "b": keys[1].Public, "c": keys[2].Public}, threshold: 2, now: func() time.Time { return now }}
	bundle := Bundle{Schema: SchemaV1, ReleaseID: "rel_2026.08.18.1", Version: "2026.08.18.1", Platform: "linux", Architecture: "amd64", Action: "promote", PolicyRevision: 2, RolloutPercentage: 5, TUFIndexTarget: "release-index-stable-linux-amd64.json", TUFIndexSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AuthorityRequestID: "rar_0123456789abcdef", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	payload, err := canonical(bundle)
	if err != nil {
		t.Fatal(err)
	}
	message := append([]byte("paperboat.release-authority.v1\x00"), payload...)
	bundle.Signatures = []Signature{{KeyID: "a", Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private[0], message))}, {KeyID: "b", Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private[1], message))}}
	if err := service.Verify(bundle); err != nil {
		t.Fatal(err)
	}
	bundle.Signatures = bundle.Signatures[:1]
	if err := service.Verify(bundle); !errors.Is(err, ErrSignature) {
		t.Fatalf("error=%v", err)
	}
}

func TestRequestUsesCurrentAdminRoleAndConcreteAuditWriter(t *testing.T) {
	store := openReleaseAuthorityTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	actor := "release_admin_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$2,'unused test login','active','admin')`, actor, suffix+"@release.test"); err != nil {
		t.Fatal(err)
	}
	keys := make([]Key, 2)
	for i := range keys {
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = Key{ID: fmt.Sprintf("key-%d", i), Public: public}
	}
	service, err := New(store, nil, Config{Keys: keys, Threshold: 2})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Action: "promote", ReleaseID: "release_" + suffix, Version: "1.0.0.0", Platform: "linux", Architecture: "amd64", PolicyRevision: 1, RolloutPercentage: 5}
	operationKey := "release-request-" + suffix
	created, err := service.Request(ctx, actor, operationKey, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Request(ctx, actor, operationKey, request)
	if err != nil || replay.ID != created.ID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	var audits int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='release_authority.requested' AND resource_id=$1 AND idempotency_key=$2`, created.ID, operationKey).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count=%d err=%v", audits, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='support' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Request(ctx, actor, operationKey, request); !errors.Is(err, ErrForbidden) {
		t.Fatalf("demoted replay error=%v, want ErrForbidden", err)
	}
}

func openReleaseAuthorityTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run release-authority PostgreSQL integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDecodeRejectsDuplicateAndUnknownFields(t *testing.T) {
	if _, err := Decode([]byte(`{"schema":"x","schema":"y"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate error=%v", err)
	}
	if _, err := Decode([]byte(`{"unknown":true}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown error=%v", err)
	}
}

func TestReleaseAuthorityRejectsDarwinAMD64(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	service := &Service{now: func() time.Time { return now }}
	bundle := Bundle{Schema: SchemaV1, ReleaseID: "rel_2026.08.18.1", Version: "2026.08.18.1", Platform: "darwin", Architecture: "amd64", Action: "promote", PolicyRevision: 2, RolloutPercentage: 5, TUFIndexTarget: "release-index-stable-darwin-amd64.json", TUFIndexSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AuthorityRequestID: "rar_0123456789abcdef", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	if err := service.Verify(bundle); !errors.Is(err, ErrInvalid) {
		t.Fatalf("darwin amd64 bundle error = %v", err)
	}
	request := Request{Action: "promote", ReleaseID: bundle.ReleaseID, Version: bundle.Version, Platform: bundle.Platform, Architecture: bundle.Architecture, PolicyRevision: 2, RolloutPercentage: 5}
	if _, err := service.Request(t.Context(), "admin", "request-1", request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("darwin amd64 request error = %v", err)
	}
}
