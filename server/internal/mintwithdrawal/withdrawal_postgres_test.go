package mintwithdrawal

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

func key(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune('a'+seed)), ed25519.SeedSize)))
}

// F21-08: a key withdrawn while the server runs is refused by the server's own verifier at once, with
// no restart, and published in withdrawn_key_ids for edges.
func TestWithdrawnKeyIsRefusedWithoutRestartOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	previousID, currentID := "previous_"+stamp, "current_"+stamp
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.mint_withdrawn_keys WHERE key_id = $1`, previousID)
	})

	now := time.Now().UTC()
	const issuer = "https://api.example.test"
	input := mint.CredentialInput{Issuer: issuer, Audience: "paperboat-enrollment", Subject: "env_1", JTI: "jti_" + stamp, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "helper_enrollment", Scopes: []string{"helper:enroll"}, EnvironmentID: "env_1", EnrollmentID: "enr_1"}
	previous, err := mint.New([]mint.Key{{ID: previousID, PrivateKey: key(1)}}, previousID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := previous.SignCredential(input)
	if err != nil {
		t.Fatal(err)
	}
	server, err := mint.New([]mint.Key{{ID: currentID, PrivateKey: key(2)}, {ID: previousID, PrivateKey: key(1)}}, currentID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	watcher := NewWatcher(database.Pool(), database.Direct(), server)
	go func() {
		for ctx.Err() == nil {
			_ = watcher.ListenFor(ctx, 10*time.Second)
		}
	}()
	if _, err = server.VerifyCredential(token, issuer, "helper_enrollment", now); err != nil {
		t.Fatalf("previous key before withdrawal = %v", err)
	}

	if err = Withdraw(ctx, database.Pool(), currentID, currentID, "user_admin", "test"); err == nil {
		t.Fatal("active key withdrawn")
	}
	if err = Withdraw(ctx, database.Pool(), previousID, currentID, "user_admin", "leaked in a test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var verr mint.CredentialVerificationError
	for {
		_, err = server.VerifyCredential(token, issuer, "helper_enrollment", now)
		if errors.As(err, &verr) && verr.Code == mint.CredentialKeyUnknown {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("withdrawn key still verifies 5s after withdrawal: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	body, _ := io.ReadAll(recorder.Result().Body)
	if !strings.Contains(string(body), `"withdrawn_key_ids":["`+previousID+`"]`) {
		t.Fatalf("trust document does not publish the withdrawal: %s", body)
	}

	// A server that starts later applies the stored set before it serves.
	restarted, err := mint.New([]mint.Key{{ID: currentID, PrivateKey: key(2)}, {ID: previousID, PrivateKey: key(1)}}, currentID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = NewWatcher(database.Pool(), database.Direct(), restarted).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.VerifyCredential(token, issuer, "helper_enrollment", now); !errors.As(err, &verr) || verr.Code != mint.CredentialKeyUnknown {
		t.Fatalf("new server verification = %v, want %s", err, mint.CredentialKeyUnknown)
	}
}

func TestWithdrawRequiresKeyActorAndReason(t *testing.T) {
	for _, tc := range []struct{ key, actor, reason string }{{"", "user_admin", "why"}, {"old", "", "why"}, {"old", "user_admin", " "}, {"active", "user_admin", "why"}} {
		if err := Withdraw(context.Background(), nil, tc.key, "active", tc.actor, tc.reason); err == nil {
			t.Fatalf("Withdraw(%q, %q, %q) accepted", tc.key, tc.actor, tc.reason)
		}
	}
}
