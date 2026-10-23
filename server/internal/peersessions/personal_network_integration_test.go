package peersessions

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

// More than 64 computers in one account still get a configuration naming every
// other computer (production failed every endpoint of a 69-endpoint account).
func TestPersonalNetworkCarriesEveryComputerOfALargeAccountOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
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
	defer store.Close()
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(time.Now().UnixNano())
	account := "personal_network_" + suffix
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "personal-network", PrivateKey: signer}}, "personal-network", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// Controlled enrollment records contain real signed endpoint certificates.
	// All network registration/configuration below uses the production service.
	var machines []string
	for i := range 140 {
		machines = append(machines, fmt.Sprintf("personal_%02d_%s", i, suffix))
	}
	for _, machine := range machines {
		environment := "env_" + machine
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, account); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,$1,'linux','amd64','/workspace','online','occupied',true,1)`, machine, account, environment); err != nil {
			t.Fatal(err)
		}
		_, _, quic := networkEndpointTLS(t, machine, now)
		certificate, fingerprint := networkCertificateWithKeys(t, signer, account, wire.RoleMachine, machine, 1, 1, quic, now, now.Add(time.Hour))
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,'machine',1,1,$4,$5,$6,$7,'personal-network',$8)`, fingerprint[:], account, machine, certificate, quic[:], now, now.Add(time.Hour), signer.Public().(ed25519.PublicKey)); err != nil {
			t.Fatal(err)
		}
		private, public := networkX25519Key(t)
		if _, err := service.Register(ctx, NetworkRegistration{OperationID: "register_" + machine, UserID: account, EndpointID: machine, Role: "machine", MachineID: machine, EndpointGeneration: 1, MachineGeneration: 1, WireGuardPublicKey: public, DiscoPublicKey: networkDiscoPublic(t, private), QUICCertificateFingerprint: fingerprint[:]}); err != nil {
			t.Fatal(err)
		}
	}
	configuration := func(machine string) NetworkConfigResult {
		t.Helper()
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_" + machine, UserID: account, EndpointID: machine})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := configuration(machines[0]).Configuration
	if len(first) <= 128<<10 {
		t.Fatalf("140-computer configuration is %d bytes; the test must exceed the old 128 KiB bound", len(first))
	}
	claims := networkClaims(t, first)
	if peers := claims["peers"].([]any); len(peers) != len(machines)-1 {
		t.Fatalf("configuration names %d peers, want %d", len(peers), len(machines)-1)
	}
	// A wake burst: several configuration requests for one endpoint at once all succeed.
	errs := make(chan error, 6)
	for i := range 6 {
		go func() {
			_, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: fmt.Sprintf("burst_%d_%s", i, suffix), UserID: account, EndpointID: machines[1]})
			errs <- err
		}()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent configuration failed: %v", err)
		}
	}
}

func TestPersonalNetworkAdmissionOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
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
	defer store.Close()
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(time.Now().UnixNano())
	account := "personal_network_" + suffix
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "personal-network", PrivateKey: signer}}, "personal-network", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// Controlled enrollment records contain real signed endpoint certificates.
	// All network registration/configuration below uses the production service.
	machines := []string{"personal_a_" + suffix, "personal_b_" + suffix}
	for _, machine := range machines {
		environment := "env_" + machine
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, account); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,$1,'linux','amd64','/workspace','online','occupied',true,1)`, machine, account, environment); err != nil {
			t.Fatal(err)
		}
		_, _, quic := networkEndpointTLS(t, machine, now)
		certificate, fingerprint := networkCertificateWithKeys(t, signer, account, wire.RoleMachine, machine, 1, 1, quic, now, now.Add(time.Hour))
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,'machine',1,1,$4,$5,$6,$7,'personal-network',$8)`, fingerprint[:], account, machine, certificate, quic[:], now, now.Add(time.Hour), signer.Public().(ed25519.PublicKey)); err != nil {
			t.Fatal(err)
		}
		private, public := networkX25519Key(t)
		if _, err := service.Register(ctx, NetworkRegistration{OperationID: "register_" + machine, UserID: account, EndpointID: machine, Role: "machine", MachineID: machine, EndpointGeneration: 1, MachineGeneration: 1, WireGuardPublicKey: public, DiscoPublicKey: networkDiscoPublic(t, private), QUICCertificateFingerprint: fingerprint[:]}); err != nil {
			t.Fatal(err)
		}
	}
	configuration := func(machine string) NetworkConfigResult {
		t.Helper()
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_" + machine, UserID: account, EndpointID: machine})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	a, b := configuration(machines[0]), configuration(machines[1])
	machineService := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{}, nil)
	addresses := map[string]bool{}
	for _, machine := range machines {
		item, err := machineService.Get(ctx, account, machine)
		if err != nil {
			t.Fatal(err)
		}
		if !item.NetworkApproved || item.AssignedIP == "" || addresses[item.AssignedIP] {
			t.Fatal("machine inventory lacks unique approved canonical addresses")
		}
		addresses[item.AssignedIP] = true
		again, err := machineService.Get(ctx, account, machine)
		if err != nil || again.AssignedIP != item.AssignedIP {
			t.Fatal("machine address changed during inventory refresh", err)
		}
	}
	assertNetworkConfig(t, a.Configuration, machines[0], machines[1], "dial", 1)
	assertNetworkConfig(t, b.Configuration, machines[1], machines[0], "accept", 1)
	scope := func(token string) map[string]any {
		return networkClaims(t, token)["peers"].([]any)[0].(map[string]any)["scopes"].([]any)[0].(map[string]any)
	}
	first, second := scope(a.Configuration), scope(b.Configuration)
	if first["resource_kind"] != "machine_network" || first["capability"] != "connect" || first["resource_id"] != second["resource_id"] {
		t.Fatal("session-free machines did not receive reciprocal network-only scopes")
	}
	claims := networkClaims(t, a.Configuration)
	if claims["exp"].(float64)-claims["iat"].(float64) > 300 {
		t.Fatal("network admission exceeded five-minute validity")
	}
	// Concurrent refreshes must commit distinct generations without 503-causing
	// serializable errors. No access sessions or application grants are created.
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	generations := make(chan float64, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: fmt.Sprintf("concurrent_%s_%d", suffix, i), UserID: account, EndpointID: machines[0]})
			if err != nil {
				errors <- err
				return
			}
			generations <- networkClaims(t, result.Configuration)["generation"].(float64)
		}()
	}
	wg.Wait()
	close(errors)
	close(generations)
	for err := range errors {
		t.Fatalf("concurrent configuration: %v", err)
	}
	seen := map[float64]bool{}
	for generation := range generations {
		if seen[generation] {
			t.Fatal("configuration generation reused")
		}
		seen[generation] = true
	}
	if len(seen) != 8 {
		t.Fatal("concurrent refresh did not complete")
	}
	var sessions int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_id=$1`, account).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("network connectivity created application sessions", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=2 WHERE id=$1`, machines[1]); err != nil {
		t.Fatal(err)
	}
	assertNetworkConfig(t, configuration(machines[0]).Configuration, machines[0], "", "", 0)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=1 WHERE id=$1`, machines[1]); err != nil {
		t.Fatal(err)
	}
	team := "personal_team_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, account); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, machines[1], team); err != nil {
		t.Fatal(err)
	}
	assertNetworkConfig(t, configuration(machines[0]).Configuration, machines[0], "", "", 0)
	assertNetworkConfig(t, configuration(machines[1]).Configuration, machines[1], "", "", 0)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=NULL WHERE id=$1`, machines[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.peer_network_identities SET revoked_at=now() WHERE endpoint_id=$1`, machines[1]); err != nil {
		t.Fatal(err)
	}
	assertNetworkConfig(t, configuration(machines[0]).Configuration, machines[0], "", "", 0)
}
