package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/peeridentity"
)

func TestNetworkCertificateRebindKeepsWireGuardKeyPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run network rebind")
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	userID, cliID := "rebind_user_"+suffix, "rebind_cli_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$3,'rebind test','cli','linux',ARRAY['machines:connect'],'active',$4,$4)`, cliID, userID, "client_"+suffix, now); err != nil {
		t.Fatal(err)
	}
	signer, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	quicPublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var quic [32]byte
	copy(quic[:], quicPublic)
	keyID, err := peeridentity.KeyID(signer)
	if err != nil {
		t.Fatal(err)
	}
	insertCertificate := func(serial int64) [sha256.Size]byte {
		t.Helper()
		payload := []byte{'P', 'B', 'E', 'C', 1}
		payload = binary.BigEndian.AppendUint16(payload, uint16(len(userID)))
		payload = append(payload, userID...)
		payload = append(payload, byte(wire.RoleCLI))
		payload = binary.BigEndian.AppendUint16(payload, uint16(len(cliID)))
		payload = append(payload, cliID...)
		payload = append(payload, quic[:]...)
		payload = binary.BigEndian.AppendUint64(payload, 1)
		payload = binary.BigEndian.AppendUint64(payload, uint64(serial))
		payload = binary.BigEndian.AppendUint64(payload, uint64(now.Add(-time.Minute).Unix()))
		payload = binary.BigEndian.AppendUint64(payload, uint64(now.Add(time.Hour).Unix()))
		raw := append(payload, ed25519.Sign(private, payload)...)
		fingerprint := sha256.Sum256(raw)
		parsed, err := wire.Verify(raw, signer, wire.Expected{AccountID: userID, Role: wire.RoleCLI, EndpointID: cliID, Generation: 1, Serial: uint64(serial)}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,key_id,endpoint_id,role,generation,serial,certificate,quic_public_key,signer_public_key,issued_at,expires_at) VALUES($1,$2,$3,$4,'cli',1,$5,$6,$7,$8,$9,$10)`, fingerprint[:], userID, keyID, cliID, serial, raw, parsed.QUICPublicKey[:], signer, now.Add(-time.Minute), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return fingerprint
	}
	first := insertCertificate(1)
	_, signingPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "rebind-test", PrivateKey: signingPrivate}}, "rebind-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	wgPrivate, wgPublic := networkX25519Key(t)
	disco := networkDiscoPublic(t, wgPrivate)
	initial, err := service.Register(ctx, NetworkRegistration{OperationID: "operation_rebind_first_" + suffix, UserID: userID, EndpointID: cliID, Role: "cli", EndpointGeneration: 1, WireGuardPublicKey: wgPublic, DiscoPublicKey: disco, QUICCertificateFingerprint: first[:]})
	if err != nil || initial.KeyGeneration != 1 {
		t.Fatalf("initial network registration: %+v %v", initial, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.peer_endpoint_certificates SET revoked_at=$1,revocation_reason='certificate_superseded' WHERE fingerprint=$2`, now, first[:]); err != nil {
		t.Fatal(err)
	}
	second := insertCertificate(2)
	rebound, err := service.Register(ctx, NetworkRegistration{OperationID: "operation_rebind_second_" + suffix, UserID: userID, EndpointID: cliID, Role: "cli", EndpointGeneration: 1, ExpectedKeyGeneration: 1, WireGuardPublicKey: wgPublic, DiscoPublicKey: disco, QUICCertificateFingerprint: second[:]})
	if err != nil || rebound.KeyGeneration != 1 || rebound.VirtualAddress != initial.VirtualAddress {
		t.Fatalf("certificate rebind changed network key: %+v %v", rebound, err)
	}
	var stored []byte
	if err := store.SQL().QueryRowContext(ctx, `SELECT quic_certificate_fingerprint FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, userID, cliID).Scan(&stored); err != nil || string(stored) != string(second[:]) {
		t.Fatalf("new certificate fingerprint was not bound: %v", err)
	}
	if _, err := service.Register(ctx, NetworkRegistration{OperationID: "operation_rebind_old_" + suffix, UserID: userID, EndpointID: cliID, Role: "cli", EndpointGeneration: 1, ExpectedKeyGeneration: 1, WireGuardPublicKey: wgPublic, DiscoPublicKey: disco, QUICCertificateFingerprint: first[:]}); err != ErrDenied {
		t.Fatalf("revoked certificate regained network authority: %v", err)
	}
}
