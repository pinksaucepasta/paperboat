package selfhost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Exercises the packaged production pbh executable and actual pinned TLS against
// an isolated PostgreSQL database. No claim endpoint or authorization is mocked.
func TestPackagedHelperEnrollmentOnPostgres(t *testing.T) {
	dsn, binary := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"), os.Getenv("PBH_TEST_BINARY")
	if dsn == "" || binary == "" {
		t.Skip("requires isolated PostgreSQL and packaged pbh")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	account, other := "enroll_"+randomIdentity(), "other_"+randomIdentity()
	for _, id := range []string{account, other} {
		if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id IN($1,$2)`, account, other)
	service := New(store, "selfhost-test-encryption-key-32-bytes-long")
	service.ControlURL = "https://control.example.test"
	service.TrustDocuments = func(context.Context) ([]byte, []byte, error) {
		return json.RawMessage(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"claim-test","x":"dGVzdA"}]}`), json.RawMessage(`{"version":1,"revocations":[]}`), nil
	}
	newHelper := func() (string, string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		listen, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listen.Addr().String()
		listen.Close()
		command := exec.Command(binary, "install", "--no-service", "--state-dir", dir, "--endpoint-host", "140.245.11.41", "--claim-listen", address, "--enable", "both")
		if output, e := command.CombinedOutput(); e != nil {
			t.Fatalf("initialize production helper: %v: %s", e, output)
		}
		command = exec.Command(binary, "selfhost", "code", "--state-dir", dir)
		out, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		var code string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Self-host code: ") {
				code = strings.TrimPrefix(line, "Self-host code: ")
			}
		}
		if code == "" {
			t.Fatal("helper omitted code")
		}
		command = exec.Command(binary, "serve", "--state-dir", dir, "--binary-dir", filepath.Dir(binary))
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			command.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				command.Process.Kill()
				<-done
			}
		})
		deadline := time.Now().Add(3 * time.Second)
		for {
			c, e := net.DialTimeout("tcp", address, 50*time.Millisecond)
			if e == nil {
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("helper claim listener unavailable")
			}
			time.Sleep(20 * time.Millisecond)
		}
		return address, code
	}
	address, code := newHelper()
	// The override confines this test's real network connection to its owned
	// ephemeral listener; production still refuses private-address SSRF targets.
	service.DialAddress = func(context.Context, string) (string, error) { return address, nil }
	request := EnrollRequest{Address: address, Code: code, Scope: Scope{Kind: "account", ID: account}}
	if _, err = service.Enroll(ctx, other, request, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-account claim authorized: %v", err)
	}
	bad := request
	bad.Code = code[:len(code)-1] + "!"
	if _, err = service.Enroll(ctx, account, bad, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid code accepted: %v", err)
	}
	installs, err := service.Enroll(ctx, account, request, false)
	if err != nil {
		t.Fatalf("real helper claim: %v", err)
	}
	if len(installs) != 2 {
		t.Fatalf("dual-component claim returned %d nodes", len(installs))
	}
	defer func() {
		for _, i := range installs {
			store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, i.NodeID)
			store.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE id=$1`, i.InstallationID)
			store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, i.NodeID)
		}
	}()
	for _, i := range installs {
		if i.ScopeKind != "account" || i.ScopeID != account {
			t.Fatal("wrong ownership")
		}
		var eligible, outsider bool
		if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,$3),paperboat.node_pool_eligible($1,$4,$3)`, i.NodeID, account, i.Capability, other).Scan(&eligible, &outsider); err != nil {
			t.Fatal(err)
		}
		if !eligible || outsider {
			t.Fatal("scope eligibility violated")
		}
		if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET node_generation=node_generation+1 WHERE id=$1`, i.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	// Receipt retry must retain the original claim generation after runtime registration.
	retry, err := service.Enroll(ctx, account, request, false)
	if err != nil || len(retry) != 2 {
		t.Fatalf("durable same-owner receipt retry: %v", err)
	}
	claimOther := request
	claimOther.Scope = Scope{Kind: "account", ID: other}
	if _, err = service.Enroll(ctx, other, claimOther, false); !errors.Is(err, ErrUsed) {
		t.Fatalf("single-use owner fence: %v", err)
	}
	global := request
	global.Scope = Scope{Kind: "global"}
	if _, err = service.Enroll(ctx, account, global, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("customer claimed global node: %v", err)
	}
	if err = service.RemoveScoped(ctx, other, installs[0].InstallationID, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("outsider removal: %v", err)
	}
	if err = service.RemoveScoped(ctx, account, installs[0].InstallationID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Enroll(ctx, account, request, false); !errors.Is(err, ErrRevoked) {
		t.Fatalf("removed claim resumed: %v", err)
	}
}
