package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func banTestStore(t *testing.T) *db.DB {
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
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

// A banned account, its WorkOS subject and its e-mail (any case, any +suffix) cannot sign in, and cannot sign up again with
// the same subject or e-mail; lifting the ban lets them in. A blocked network cannot sign in or up at all.
func TestBannedIdentityAndBlockedNetworkCannotSignInPostgres(t *testing.T) {
	store := banTestStore(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("ban%d", time.Now().UnixNano())
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.bans WHERE reason=$1`, prefix)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.ip_blocks WHERE reason=$1`, prefix)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE workos_subject LIKE $1`, prefix+"%")
	})
	s := NewService(store, nil, FakeWorkOSVerifier{}, []string{"test-ban-key"}, true)
	login := func(ctx context.Context, subject, email string) (User, error) {
		service := *s
		service.verifier = sessionLogoutVerifier{profile: WorkOSProfile{Subject: prefix + subject, Email: email, EmailVerified: true}}
		user, _, err := service.VerifyCallback(ctx, CallbackInput{Code: "verified-provider-code"})
		return user, err
	}
	canonical := prefix + ".person@example.test"
	user, err := login(ctx, "main", canonical)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.bans(id,subject_kind,subject_id,reason,banned_by,canonical_email,workos_subject) VALUES($1,'user',$2,$3,'operator_x',$4,$5)`, "ban_"+prefix, user.ID, prefix, canonical, prefix+"main")

	banned := map[string][2]string{
		"same subject":             {"main", canonical},
		"alias of the e-mail":      {"alias", strings.ToUpper(prefix) + ".Person+work@EXAMPLE.TEST"},
		"same subject, new e-mail": {"main", prefix + ".other@example.test"},
		"new subject, same e-mail": {"fresh", canonical},
	}
	for name, c := range banned {
		if _, err := login(ctx, c[0], c[1]); !errors.Is(err, ErrAccountBanned) {
			t.Fatalf("%s: %v, want ErrAccountBanned", name, err)
		}
	}
	// A sign-up of an identity whose account no longer exists is refused as well, and creates no account.
	gone := prefix + ".gone@example.test"
	exec(`INSERT INTO paperboat.bans(id,subject_kind,subject_id,reason,banned_by,canonical_email,workos_subject) VALUES($1,'user','user_gone_'||$2,$2,'operator_x',$3,$4)`, "ban_gone_"+prefix, prefix, gone, prefix+"gone")
	for name, c := range map[string][2]string{"its e-mail": {"newsubject", strings.ToUpper(gone)}, "its subject": {"gone", prefix + ".elsewhere@example.test"}} {
		if _, err := login(ctx, c[0], c[1]); !errors.Is(err, ErrAccountBanned) {
			t.Fatalf("sign-up with %s: %v", name, err)
		}
	}
	var created int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.users WHERE workos_subject IN ($1,$2) OR canonical_email=$3`, prefix+"gone", prefix+"newsubject", gone).Scan(&created); err != nil || created != 0 {
		t.Fatalf("a refused sign-up created %d accounts (%v)", created, err)
	}
	// Someone else signs up as usual.
	if _, err := login(ctx, "other", prefix+".stranger@example.test"); err != nil {
		t.Fatalf("an unrelated sign-up: %v", err)
	}
	exec(`UPDATE paperboat.bans SET lifted_at=now(),lifted_by='operator_x' WHERE id=$1`, "ban_"+prefix)
	if _, err := login(ctx, "main", canonical); err != nil {
		t.Fatalf("sign-in after the ban was lifted: %v", err)
	}

	// A network block refuses sign-in and sign-up from inside the range (IPv4, IPv6, IPv4-mapped), nothing else, and not after expiry.
	exec(`INSERT INTO paperboat.ip_blocks(id,cidr,reason,created_by) VALUES('ipb_'||$1||'4','203.0.113.0/24',$1,'operator_x'),('ipb_'||$1||'6','2001:db8:77::/48',$1,'operator_x')`, prefix)
	from := func(ip string) context.Context { return audit.WithClient(ctx, audit.Client{IP: ip}) }
	for _, ip := range []string{"203.0.113.9", "2001:db8:77:5::1", "::ffff:203.0.113.200"} {
		if _, err := login(from(ip), "main", canonical); !errors.Is(err, ErrIPBlocked) {
			t.Fatalf("from %s: %v, want ErrIPBlocked", ip, err)
		}
		if _, err := login(from(ip), "newcomer", prefix+".new@example.test"); !errors.Is(err, ErrIPBlocked) {
			t.Fatalf("sign-up from %s: %v, want ErrIPBlocked", ip, err)
		}
	}
	for _, ip := range []string{"203.0.114.9", "2001:db8:78::1", "198.51.100.4", ""} {
		if _, err := login(from(ip), "main", canonical); err != nil {
			t.Fatalf("from %q: %v", ip, err)
		}
	}
	exec(`UPDATE paperboat.ip_blocks SET expires_at=now()-interval '1 second' WHERE reason=$1`, prefix)
	if _, err := login(from("203.0.113.9"), "main", canonical); err != nil {
		t.Fatalf("an expired block still refuses: %v", err)
	}
}

// The CLI login refuses a blocked network at the start and at the token poll, with a clear code.
func TestDeviceLoginRefusesBlockedNetworkPostgres(t *testing.T) {
	store := banTestStore(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("devban%d", time.Now().UnixNano())
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.ip_blocks WHERE reason=$1`, prefix)
	})
	cfg := config.Default().CLIAuth
	service := NewDeviceService(store, nil, cfg, []string{"device-ban-key"})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.ip_blocks(id,cidr,reason,created_by) VALUES('ipb_'||$1,'198.51.100.0/24',$1,'operator_x')`, prefix); err != nil {
		t.Fatal(err)
	}
	in := DeviceAuthorizationInput{ClientID: cfg.ClientID, ClientLabel: "laptop", DeviceType: "desktop", OS: "linux", Scopes: cfg.AllowedScopes, Network: "198.51.100.77"}
	var e *DeviceError
	if _, err := service.Authorize(ctx, in); !errors.As(err, &e) || e.Code != "ip_blocked" {
		t.Fatalf("authorize from a blocked network: %v", err)
	}
	if _, err := service.Poll(ctx, DeviceTokenInput{ClientID: cfg.ClientID, DeviceCode: "x", Network: "198.51.100.77"}); !errors.As(err, &e) || e.Code != "ip_blocked" {
		t.Fatalf("poll from a blocked network: %v", err)
	}
	// API-token and recipe sessions are CLI logins too.
	blocked := audit.WithClient(ctx, audit.Client{IP: "198.51.100.77"})
	if _, err := service.StartAPITokenSession(blocked, APITokenSessionRequest{TokenID: "tok_x", Account: "usr_x", Label: "ci", DeviceType: "server", OS: "linux"}); !errors.As(err, &e) || e.Code != "ip_blocked" {
		t.Fatalf("an API-token session from a blocked network: %v", err)
	}
	if _, err := service.StartRecipeSession(blocked, RecipeSessionRequest{TokenID: strings.Repeat("a", 10), Auth: make([]byte, 32), Label: "ci", DeviceType: "server", OS: "linux"}); !errors.As(err, &e) || e.Code != "ip_blocked" {
		t.Fatalf("a recipe session from a blocked network: %v", err)
	}
	in.Network = "203.0.113.5"
	if _, err := service.Authorize(ctx, in); err != nil {
		t.Fatalf("authorize from another network: %v", err)
	}
}
