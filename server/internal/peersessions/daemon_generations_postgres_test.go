package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat/server/internal/retention"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

// Each user daemon generation of one CLI session has its own network identity, and the machine it has access to
// sees every live one with the session's grant: an upgraded daemon serves new connects while the old one keeps
// what it holds. A crash restart reuses its row, drain and withdraw are idempotent, a fifth generation evicts the
// oldest (which learns why), and ending the session revokes them all.
func TestDaemonGenerationsShareTheSessionAndRetirePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run daemon generations")
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
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	userID, cliID, machineID, envID := "gen_user_"+suffix, "gen_cli_"+suffix, "gen_machine_"+suffix, "gen_env_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test")
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$3,'generations','desktop','test',ARRAY['machines:connect'],'active',$4,$4)`, cliID, userID, "client_"+suffix, now)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$2,$3)`, envID, "workspace_"+suffix, userID)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machineID, userID, envID)

	identityRepo, err := peeridentity.NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	identityService, err := peeridentity.NewService(identityRepo)
	if err != nil {
		t.Fatal(err)
	}
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	keyID, err := peeridentity.KeyID(rootPublic)
	if err != nil {
		t.Fatal(err)
	}
	_, _, cliQUIC := networkEndpointTLS(t, "cli", now)
	cliRaw, cliFingerprint := networkCertificateWithKeys(t, rootPrivate, userID, wire.RoleCLI, cliID, 1, 1, cliQUIC, now, now.Add(time.Hour))
	if _, err = identityService.Bootstrap(ctx, peeridentity.BootstrapRequest{CLIClientSessionID: cliID, RootPublicKey: rootPublic, RegisterRequest: peeridentity.RegisterRequest{OperationID: "operation_bootstrap_" + suffix, UserID: userID, KeyID: keyID, Certificate: cliRaw, Expected: wire.Expected{AccountID: userID, Role: wire.RoleCLI, EndpointID: cliID, Generation: 1, Serial: 1}, ExpectedRootFingerprint: rootFingerprint, ExpectedCertificateFingerprint: cliFingerprint, ExpectedIssuedAt: now, ExpectedExpiresAt: now.Add(time.Hour), Now: now}}); err != nil {
		t.Fatal(err)
	}
	machineRequest, err := identityService.RequestMachineEndpoint(ctx, peeridentity.MachineEndpointRequest{OperationID: "operation_machine_request_" + suffix, UserID: userID, EndpointID: machineID, Generation: 1, QUICPublicKey: func() [32]byte { _, _, q := networkEndpointTLS(t, "machine", now); return q }(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	machineRaw, machineFingerprint := networkCertificateWithKeys(t, rootPrivate, userID, wire.RoleMachine, machineID, 1, 1, machineRequest.QUICPublicKey, now, now.Add(time.Hour))
	if _, err = identityService.Register(ctx, peeridentity.RegisterRequest{OperationID: "operation_machine_certificate_" + suffix, UserID: userID, KeyID: keyID, Certificate: machineRaw, Expected: wire.Expected{AccountID: userID, Role: wire.RoleMachine, EndpointID: machineID, Generation: 1, Serial: 1}, ExpectedRootFingerprint: rootFingerprint, ExpectedCertificateFingerprint: machineFingerprint, ExpectedIssuedAt: now, ExpectedExpiresAt: now.Add(time.Hour), Now: now}); err != nil {
		t.Fatal(err)
	}
	_, signingPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "generations-test", PrivateKey: signingPrivate}}, "generations-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	machinePrivate, machinePublic := networkX25519Key(t)
	if _, err = service.Register(ctx, NetworkRegistration{OperationID: "operation_machine_" + suffix, UserID: userID, EndpointID: machineID, OwnerEndpointID: machineID, Role: "machine", MachineID: machineID, EndpointGeneration: 1, MachineGeneration: 1, WireGuardPublicKey: machinePublic, DiscoPublicKey: networkDiscoPublic(t, machinePrivate), QUICCertificateFingerprint: machineFingerprint[:]}); err != nil {
		t.Fatal(err)
	}
	accessID := "gen_access_" + suffix
	exec(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at) VALUES('external_issuer',$1,$2,$3,$4,$5,'https://machine.example.test',$6,$7,ARRAY['terminal','private_access'],$8,$9)`, accessID, machineID, userID, envID, cliID, "jti_terminal_"+suffix, "jti_file_"+suffix, "operation_access_"+suffix, now.Add(time.Hour))

	generation := func(i int) string { return fmt.Sprintf("%032x", i+1) }
	endpoint := func(i int) string {
		e, ok := peernetwork.DaemonEndpointID(cliID, generation(i))
		if !ok {
			t.Fatal("daemon endpoint")
		}
		return e
	}
	keys := map[int][]byte{}
	operations := 0
	register := func(i int, expected int64) NetworkRegistrationResult {
		t.Helper()
		if keys[i] == nil {
			private, public := networkX25519Key(t)
			keys[i] = append(public, networkDiscoPublic(t, private)...)
		}
		operations++
		result, err := service.Register(ctx, NetworkRegistration{OperationID: fmt.Sprintf("operation_gen_%d_%s", operations, suffix), UserID: userID, EndpointID: endpoint(i), OwnerEndpointID: cliID, Role: "cli", EndpointGeneration: 1, ExpectedKeyGeneration: expected, WireGuardPublicKey: keys[i][:32], DiscoPublicKey: keys[i][32:], QUICCertificateFingerprint: cliFingerprint[:]})
		if err != nil {
			t.Fatalf("register generation %d: %v", i, err)
		}
		return result
	}
	rows := func() int {
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.peer_network_identities WHERE user_id=$1 AND owner_endpoint_id=$2`, userID, cliID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	machinePeers := func() []string {
		t.Helper()
		operations++
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: fmt.Sprintf("operation_machine_config_%d_%s", operations, suffix), UserID: userID, EndpointID: machineID})
		if err != nil {
			t.Fatal(err)
		}
		var peers []string
		for _, raw := range networkClaims(t, result.Configuration)["peers"].([]any) {
			peer := raw.(map[string]any)
			id := peer["identity"].(map[string]any)["endpoint_id"].(string)
			for _, scope := range peer["scopes"].([]any) {
				if scope.(map[string]any)["resource_id"] == accessID {
					peers = append(peers, id)
					break
				}
			}
		}
		slices.Sort(peers)
		return peers
	}
	cliConfig := func(i int) error {
		operations++
		_, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: fmt.Sprintf("operation_cli_config_%d_%s", operations, suffix), UserID: userID, EndpointID: endpoint(i)})
		return err
	}

	first := register(0, 0)
	// A crash restart of the same generation registers its persisted key again: the same row, no new peer.
	if again := register(0, first.KeyGeneration); again.KeyGeneration != first.KeyGeneration || again.VirtualAddress != first.VirtualAddress || rows() != 1 {
		t.Fatalf("crash restart did not reuse the identity: first=%+v again=%+v rows=%d", first, again, rows())
	}
	second := register(1, 0)
	if second.VirtualAddress == first.VirtualAddress || len(second.EvictedEndpoints) != 0 {
		t.Fatalf("upgrade registration = %+v", second)
	}
	if got := machinePeers(); !slices.Equal(got, []string{endpoint(0), endpoint(1)}) {
		t.Fatalf("machine sees generations %v with the session's grant", got)
	}
	if err := cliConfig(0); err != nil {
		t.Fatalf("draining generation lost its configuration: %v", err)
	}

	if err := service.BeginDrain(ctx, userID, cliID, endpoint(0)); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT drain_until FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, userID, endpoint(0)).Scan(&deadline); err != nil || !deadline.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("drain deadline = %v %v", deadline, err)
	}
	service.now = func() time.Time { return now.Add(time.Minute) }
	if err := service.BeginDrain(ctx, userID, cliID, endpoint(0)); err != nil {
		t.Fatal(err)
	}
	var repeated time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT drain_until FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, userID, endpoint(0)).Scan(&repeated); err != nil || !repeated.Equal(deadline) {
		t.Fatalf("a repeated drain moved the deadline: %v %v", repeated, err)
	}
	if got := machinePeers(); !slices.Equal(got, []string{endpoint(0), endpoint(1)}) {
		t.Fatalf("draining generation dropped from the machine early: %v", got)
	}

	for range 2 {
		if err := service.Withdraw(ctx, userID, cliID, endpoint(0)); err != nil {
			t.Fatalf("withdraw: %v", err)
		}
	}
	if got := machinePeers(); !slices.Equal(got, []string{endpoint(1)}) {
		t.Fatalf("withdrawn generation still a peer: %v", got)
	}
	if err := cliConfig(0); !errors.Is(err, ErrGenerationWithdrawn) {
		t.Fatalf("withdrawn generation configuration: %v", err)
	}

	// Generations 1..4 are live; a fifth live one evicts the oldest (1).
	for i := 2; i <= 4; i++ {
		register(i, 0)
	}
	fifth := register(5, 0)
	if !slices.Equal(fifth.EvictedEndpoints, []string{endpoint(1)}) {
		t.Fatalf("evicted = %v", fifth.EvictedEndpoints)
	}
	if err := cliConfig(1); !errors.Is(err, ErrGenerationEvicted) {
		t.Fatalf("evicted generation configuration: %v", err)
	}
	if got := machinePeers(); len(got) != MaximumDaemonGenerations || slices.Contains(got, endpoint(1)) {
		t.Fatalf("machine peers after eviction: %v", got)
	}

	// Retention revokes a generation still draining at its cap.
	if err := service.BeginDrain(ctx, userID, cliID, endpoint(2)); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE paperboat.peer_network_identities SET drain_until=now()-interval '1 second' WHERE user_id=$1 AND endpoint_id=$2`, userID, endpoint(2))
	if result, err := retention.Run(ctx, store, config.Default().Retention); err != nil || result["peer_network_generations_drain_expired"] < 1 {
		t.Fatalf("retention: %v %v", result, err)
	}
	if err := cliConfig(2); !errors.Is(err, ErrGenerationWithdrawn) {
		t.Fatalf("drain-expired generation configuration: %v", err)
	}

	// Ending the session revokes every generation of it.
	exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, cliID)
	var live int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.peer_network_identities WHERE user_id=$1 AND owner_endpoint_id=$2 AND revoked_at IS NULL`, userID, cliID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live generations after the session ended: %d %v", live, err)
	}
	if got := machinePeers(); len(got) != 0 {
		t.Fatalf("machine still sees generations of an ended session: %v", got)
	}
}
