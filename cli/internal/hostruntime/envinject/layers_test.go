package envinject

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
)

type layerTestMarker struct{ established bool }

func (m *layerTestMarker) LayerGenesisEstablished() (bool, error) { return m.established, nil }
func (m *layerTestMarker) EstablishLayerGenesis() error           { m.established = true; return nil }

func TestLayerStoreWorkspaceHierarchyFloorsAndForeignActor(t *testing.T) {
	ctx := context.Background()
	material := environmentkey.Material{Generation: 7}
	copy(material.Private[:], bytes.Repeat([]byte{0x37}, 32))
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	writer := bytes.Repeat([]byte{0x49}, 32)
	defer clear(writer)
	recipient := api.VaultLayerRecipient{RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: 11, HostKeyGeneration: 7, HostPublic: base64.RawURLEncoding.EncodeToString(public[:])}
	marker := &layerTestMarker{}
	path := filepath.Join(t.TempDir(), "floor.json")
	recordPages := layerTestRecordPages{}
	config := LayerConfig{Path: path, Issuer: "https://control.example", AccountID: "account_1", MachineID: "machine_1", InstallationGeneration: 11, Keys: layerStaticHostKey{material}, Marker: marker, Records: recordPages}
	store, err := NewLayerStore(config)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(workspace, kind, owner, machine, value string, generation, fence uint64, previous env.DocumentID) api.VaultLayerDelivery {
		t.Helper()
		source := api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine}, KeyEpoch: 1, Revision: generation, DocumentID: env.DocumentID([32]byte{byte(generation)}).String()}
		id, _ := env.ParseDocumentID(source.DocumentID)
		contents := map[string][]byte{"VALUE": []byte(value)}
		if value == "wide" {
			contents = map[string][]byte{}
			for i := 0; i < 4; i++ {
				contents[fmt.Sprintf("V_%s_%s_%d", kind, machine, i)] = bytes.Repeat([]byte("x"), 31000)
			}
		}
		defer clearLayerValues(contents)
		scopeKey := bytes.Repeat([]byte{byte(7)}, 32)
		layer, err := env.SealVaultScopeKey(ctx, env.VaultLayerClaims{Issuer: config.Issuer, RecipientAccount: recipient.RecipientAccount, MachineID: recipient.MachineID, InstallationGeneration: 11, HostKeyGeneration: 7, HostPublic: public[:], DeliveryGeneration: generation, Previous: previous[:], FenceGeneration: fence, Source: env.VaultLayerSource{WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: 1, Revision: generation, Digest: id[:]}, WriterAccount: "account_1", WriterVaultGeneration: 1}, writer, scopeKey)
		if err != nil {
			t.Fatal(err)
		}

		states := []api.VaultRecordState{}
		for name, value := range contents {
			record, err := env.SealVaultRecord(ctx, env.VaultRecordClaims{Issuer: config.Issuer, WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: 1, Revision: generation, WriterAccount: "account_1", WriterVaultGeneration: 1}, scopeKey, writer, name, value)
			if err != nil {
				t.Fatal(err)
			}
			states = append(states, api.VaultRecordState{WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: 1, RecordID: record.Claims.RecordID, Revision: generation, Sequence: generation, DocumentID: record.ID.String(), WriterPublic: base64.RawURLEncoding.EncodeToString(layer.Claims.WriterPublic), Envelope: base64.RawURLEncoding.EncodeToString(record.Raw)})
		}
		recordPages[layerCoordinate(source)] = api.VaultRecordsPage{Records: states, Sequence: generation}
		r := recipient
		r.DeliveryGeneration = generation
		r.FenceGeneration = fence
		r.DocumentID = layer.ID.String()
		return api.VaultLayerDelivery{RecordSequence: generation, Recipient: r, Source: source, WriterAccount: "account_1", WriterPublic: base64.RawURLEncoding.EncodeToString(layer.Claims.WriterPublic), Envelope: base64.RawURLEncoding.EncodeToString(layer.Raw), State: "ready"}
	}
	boundedConfig := config
	boundedConfig.Path = filepath.Join(t.TempDir(), "bounded-floor.json")
	boundedConfig.Marker = &layerTestMarker{}
	boundedStore, err := NewLayerStore(boundedConfig)
	if err != nil {
		t.Fatal(err)
	}
	wide := []api.VaultLayerDelivery{seal("team_1", "team", "team_1", "", "wide", 1, 1, env.DocumentID{}), seal("team_1", "personal", "account_1", "", "wide", 1, 1, env.DocumentID{}), seal("team_1", "personal", "account_1", "machine_1", "wide", 1, 1, env.DocumentID{})}
	if _, err := boundedStore.Environment(ctx, LaunchContext{"team_1", "account_1"}, api.VaultLayerContext{Recipient: recipient, WorkspaceID: "team_1", ActorAccountID: "account_1", MachineID: "machine_1", Layers: wide}); err == nil {
		t.Fatal("merged layers exceeded existing ENV byte bound")
	}
	if _, err := os.Stat(boundedConfig.Path); !os.IsNotExist(err) {
		t.Fatal("oversized merge persisted applied floor")
	}
	team := seal("team_1", "team", "team_1", "", "team", 1, 1, env.DocumentID{})
	member := seal("team_1", "personal", "account_1", "", "member", 1, 1, env.DocumentID{})
	device := seal("team_1", "personal", "account_1", "machine_1", "device", 1, 1, env.DocumentID{})
	bundle := api.VaultLayerContext{WorkspaceID: "team_1", ActorAccountID: "account_1", MachineID: "machine_1", Recipient: recipient, Layers: []api.VaultLayerDelivery{device, team, member}}
	binding := LaunchContext{"team_1", "account_1"}
	values, err := store.Environment(ctx, binding, bundle)
	if err != nil || !reflect.DeepEqual(values, []string{"VALUE=device"}) {
		t.Fatalf("hierarchy failed: %v", err)
	}
	reports, err := store.PendingObservations(ctx)
	if err != nil || len(reports) != 3 {
		t.Fatal("applied reports not durably queued")
	}
	for _, report := range reports {
		if report.State != "applied" || report.ErrorCode != nil || report.ObservationSeq != 1 {
			t.Fatal("wrong applied report")
		}
	}
	if err := store.AcknowledgeObservations(ctx, reports); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.PendingObservations(ctx); err != nil || len(pending) != 0 {
		t.Fatal("ACK did not clear matching reports")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("VALUE")) || bytes.Contains(raw, []byte("device")) {
		t.Fatal("plaintext persisted")
	}
	bundle.Layers = []api.VaultLayerDelivery{member, team}
	values, err = store.Environment(ctx, binding, bundle)
	if err != nil || !reflect.DeepEqual(values, []string{"VALUE=member"}) {
		t.Fatal("member precedence failed")
	}
	bundle.ActorAccountID = "account_2"
	bundle.Layers = []api.VaultLayerDelivery{team}
	values, err = store.Environment(ctx, LaunchContext{"team_1", "account_2"}, bundle)
	if err != nil || !reflect.DeepEqual(values, []string{"VALUE=team"}) {
		t.Fatal("foreign actor Teamglobal failed")
	}
	bundle.Layers = []api.VaultLayerDelivery{team, member}
	if _, err := store.Environment(ctx, LaunchContext{"team_1", "account_2"}, bundle); err == nil {
		t.Fatal("foreign actor private ENV leaked")
	}
	bundle.WorkspaceID = "personal"
	bundle.ActorAccountID = "account_1"
	bundle.Layers = []api.VaultLayerDelivery{team}
	if _, err := store.Environment(ctx, LaunchContext{"personal", "account_1"}, bundle); err == nil {
		t.Fatal("Team ENV leaked into Personal")
	}
	bundle.WorkspaceID = "team_1"
	bundle.Layers = []api.VaultLayerDelivery{team}
	prior, _ := env.ParseDocumentID(team.Recipient.DocumentID)
	updated := seal("team_1", "team", "team_1", "", "new", 2, 2, prior)
	bundle.Layers = []api.VaultLayerDelivery{updated}
	if _, err := store.Environment(ctx, binding, bundle); err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeObservations(ctx, reports); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingObservations(ctx)
	if err != nil || len(pending) != 1 || pending[0].DeliveryGeneration != 2 || pending[0].ObservationSeq != 2 {
		t.Fatal("old ACK cleared newer report")
	}
	malformed := updated
	malformed.Envelope = "%"
	bundle.Layers = []api.VaultLayerDelivery{malformed}
	if _, err := store.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("malformed encrypted layer was applied")
	}
	pending, err = store.PendingObservations(ctx)
	if err != nil || len(pending) != 1 || pending[0].State != "failed" || pending[0].ErrorCode == nil || *pending[0].ErrorCode != "environment_projection_invalid" || pending[0].ObservationSeq != 3 {
		t.Fatal("malformed envelope failure was not durably observed")
	}
	corrupted := updated
	damaged, _ := base64.RawURLEncoding.Strict().DecodeString(corrupted.Envelope)
	damaged[len(damaged)-1] ^= 1
	corrupted.Envelope = base64.RawURLEncoding.EncodeToString(damaged)
	bundle.Layers = []api.VaultLayerDelivery{corrupted}
	if _, err := store.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("invalid encrypted layer applied")
	}
	pending, err = store.PendingObservations(ctx)
	if err != nil || len(pending) != 1 || pending[0].State != "failed" || pending[0].ErrorCode == nil || *pending[0].ErrorCode != "environment_projection_invalid" {
		t.Fatal("failed report missing or unsafe")
	}
	bundle.Layers = []api.VaultLayerDelivery{updated}
	if _, err := store.Environment(ctx, binding, bundle); err != nil {
		t.Fatal("valid layer did not recover")
	}
	pending, err = store.PendingObservations(ctx)
	if err != nil || len(pending) != 1 || pending[0].State != "applied" || pending[0].ObservationSeq != 4 {
		t.Fatal("recovery report did not advance sequence")
	}
	restarted, _ := NewLayerStore(config)
	bundle.Layers = []api.VaultLayerDelivery{team}
	if _, err := restarted.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("restarted runtime accepted rollback")
	}
	bundle.Layers = []api.VaultLayerDelivery{updated}
	bundle.Layers[0].State = "pending"
	if _, err := restarted.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("pending source accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	bundle.Layers = []api.VaultLayerDelivery{}
	if _, err := restarted.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("lost floors silently recreated")
	}
}

type layerStaticHostKey struct{ material environmentkey.Material }

func (k layerStaticHostKey) Load(ctx context.Context) (environmentkey.Material, error) {
	return k.material, ctx.Err()
}

// The production page interface lets this fixture supply signed ciphertext; the
// store still owns pagination integrity, decryption, and durable publication.
type layerTestRecordPages map[string]api.VaultRecordsPage

func (p layerTestRecordPages) VaultRecordPage(_ context.Context, _ LaunchContext, source api.VaultLayerSource, after, through uint64, _ string) (api.VaultRecordsPage, error) {
	page := p[layerCoordinate(source)]
	page.AfterSequence = after
	return page, nil
}
