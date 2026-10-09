package envinject

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
)

type recordPageFunc func(context.Context, LaunchContext, api.VaultLayerSource, uint64, uint64, string) (api.VaultRecordsPage, error)

func (f recordPageFunc) VaultRecordPage(c context.Context, b LaunchContext, s api.VaultLayerSource, a, w uint64, cursor string) (api.VaultRecordsPage, error) {
	return f(c, b, s, a, w, cursor)
}

func TestLayerRecordsDeltaFailureSnapshotRecoveryAndRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "records.json")
	material := environmentkey.Material{Generation: 7}
	copy(material.Private[:], bytes.Repeat([]byte{0x37}, 32))
	defer material.Destroy()
	publicBytes, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewLayerStore(LayerConfig{Path: path, Issuer: "https://control.example", AccountID: "account_1", MachineID: "machine_1", InstallationGeneration: 11, Keys: layerStaticHostKey{material}, Marker: &layerTestMarker{}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := api.VaultLayerContext{Recipient: api.VaultLayerRecipient{RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: 11, HostKeyGeneration: 7, HostPublic: base64.RawURLEncoding.EncodeToString(publicBytes[:])}, WorkspaceID: "personal", ActorAccountID: "account_1", MachineID: "machine_1"}
	public, _ := material.Public()
	writer := bytes.Repeat([]byte{0x49}, 32)
	key := bytes.Repeat([]byte{7}, 32)
	source := api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1"}, KeyEpoch: 1, Revision: 1, DocumentID: env.DocumentID([32]byte{1}).String()}
	digest, _ := env.ParseDocumentID(source.DocumentID)
	claims := env.VaultLayerClaims{Issuer: store.config.Issuer, RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: 11, HostKeyGeneration: 7, HostPublic: public[:], DeliveryGeneration: 1, Previous: make([]byte, 32), FenceGeneration: 1, Source: env.VaultLayerSource{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", KeyEpoch: 1, Revision: 1, Digest: digest[:]}, WriterAccount: "account_1", WriterVaultGeneration: 1}
	grant, err := env.SealVaultScopeKey(ctx, claims, writer, key)
	if err != nil {
		t.Fatal(err)
	}
	recipient := bundle.Recipient
	recipient.DeliveryGeneration = 1
	recipient.FenceGeneration = 1
	recipient.DocumentID = grant.ID.String()
	delivery := api.VaultLayerDelivery{Source: source, Recipient: recipient, RecordSequence: 1, WriterAccount: "account_1", WriterPublic: base64.RawURLEncoding.EncodeToString(grant.Claims.WriterPublic), Envelope: base64.RawURLEncoding.EncodeToString(grant.Raw), State: "ready"}
	bundle.Layers = []api.VaultLayerDelivery{delivery}
	seal := func(revision uint64, value string, deleted bool) api.VaultRecordState {
		t.Helper()
		record, err := env.SealVaultRecord(ctx, env.VaultRecordClaims{Issuer: store.config.Issuer, WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", KeyEpoch: source.KeyEpoch, Revision: revision, Deleted: deleted, WriterAccount: "account_1", WriterVaultGeneration: 1}, key, writer, "VALUE", []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return api.VaultRecordState{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", KeyEpoch: source.KeyEpoch, RecordID: record.Claims.RecordID, Revision: revision, Deleted: deleted, Sequence: revision, DocumentID: record.ID.String(), WriterPublic: delivery.WriterPublic, Envelope: base64.RawURLEncoding.EncodeToString(record.Raw)}
	}
	binding := LaunchContext{"personal", "account_1"}
	mode := "snapshot"
	calls := 0
	old := seal(1, "old", false)
	updated := seal(2, "new", false)
	deleted := seal(3, "", true)
	restored := seal(4, "restored", false)
	store.config.Records = recordPageFunc(func(_ context.Context, b LaunchContext, s api.VaultLayerSource, after, through uint64, cursor string) (api.VaultRecordsPage, error) {
		calls++
		if b != binding || s != source {
			t.Fatal("record reader lost exact launch coordinates")
		}
		page := api.VaultRecordsPage{Sequence: through, AfterSequence: after}
		switch mode {
		case "snapshot":
			if after != 0 || through != 1 {
				t.Fatal("wrong initial watermark")
			}
			page.Records = []api.VaultRecordState{old}
		case "fail":
			if after != 1 || through != 2 {
				t.Fatal("wrong delta watermark")
			}
			if cursor != "" {
				return page, errors.New("bounded page failure")
			}
			page.Records = []api.VaultRecordState{updated}
			page.NextCursor = "continuation"
		case "delta":
			if after != 1 {
				t.Fatal("partial delta committed")
			}
			page.Records = []api.VaultRecordState{updated}
		case "delete":
			if after != 2 {
				t.Fatal("wrong delete delta")
			}
			page.Records = []api.VaultRecordState{deleted}
		case "same-revision-substitution":
			replacement := seal(3, "", true)
			replacement.Sequence = through
			page.Records = []api.VaultRecordState{replacement}
		case "replay":
			replay := old
			replay.Sequence = through
			page.Records = []api.VaultRecordState{replay}
		case "resnapshot-replay":
			if after != 0 {
				t.Fatal("evicted ciphertext did not trigger snapshot")
			}
			replay := old
			replay.Sequence = through
			page.Records = []api.VaultRecordState{replay}
		case "expired":
			if after == 3 {
				return page, &api.APIError{Code: "record_snapshot_required"}
			}
			if after != 0 {
				t.Fatal("did not restart full snapshot")
			}
			page.Records = []api.VaultRecordState{restored}
		case "rotate":
			if after != 0 || through != 1 {
				t.Fatal("new key epoch reused old cache")
			}
			page.Records = []api.VaultRecordState{seal(1, "rotated", false)}
		case "foreign":
			bad := seal(5, "foreign", false)
			bad.WorkspaceID = "team_other"
			page.Records = []api.VaultRecordState{bad}
		default:
			t.Fatal("unexpected cache fetch")
		}
		return page, nil
	})
	launch := func(want string) {
		t.Helper()
		got, err := store.Environment(ctx, binding, bundle)
		if err != nil || !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("launch=%v err=%v", got, err)
		}
	}
	launch("VALUE=old")
	before, _ := os.ReadFile(path)
	bundle.Layers[0].RecordSequence = 2
	mode = "fail"
	if _, err := store.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("partial snapshot accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("page failure changed durable state")
	}
	mode = "delta"
	launch("VALUE=new")
	mode = "cached"
	prior := calls
	launch("VALUE=new")
	if calls != prior {
		t.Fatal("unchanged context downloaded records")
	}
	mode = "delete"
	bundle.Layers[0].RecordSequence = 3
	got, err := store.Environment(ctx, binding, bundle)
	if err != nil || len(got) != 0 {
		t.Fatal("signed tombstone not applied", err)
	}
	// A relabelled unsigned sequence cannot resurrect an older signed record.
	before, _ = os.ReadFile(path)
	mode = "replay"
	bundle.Layers[0].RecordSequence = 4
	if _, err := store.Environment(ctx, binding, bundle); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatal("signed pre-tombstone replay accepted", err)
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("replay changed durable floors")
	}
	mode = "same-revision-substitution"
	if _, err := store.Environment(ctx, binding, bundle); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatal("equal revision with a different signed document accepted", err)
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("same-revision substitution changed durable floors")
	}
	mode = "expired"
	bundle.Layers[0].RecordSequence = 4
	launch("VALUE=restored")
	before, _ = os.ReadFile(path)
	if bytes.Contains(before, []byte("restored")) || bytes.Contains(before, []byte("VALUE")) {
		t.Fatal("plaintext persisted")
	}
	// Switching away evicts ciphertext only; revisiting must retain record floors.
	emptyBundle := bundle
	emptyBundle.Layers = nil
	if _, err := store.Environment(ctx, binding, emptyBundle); err != nil {
		t.Fatal(err)
	}
	evicted, _ := os.ReadFile(path)
	mode = "resnapshot-replay"
	bundle.Layers[0].RecordSequence = 5
	if _, err := store.Environment(ctx, binding, bundle); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatal("lower revision accepted after cache eviction", err)
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(evicted, after) {
		t.Fatal("resnapshot replay changed durable floors")
	}
	// Restore the exact same signed revision through a full snapshot.
	mode = "expired"
	bundle.Layers[0].RecordSequence = 4
	launch("VALUE=restored")
	before, _ = os.ReadFile(path)
	mode = "foreign"
	bundle.Layers[0].RecordSequence = 5
	if _, err := store.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("foreign workspace accepted")
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("foreign record changed durable cache")
	}
	mode = "cached"
	bundle.Layers[0].RecordSequence = 3
	if _, err := store.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("record sequence rollback accepted")
	}
	// A validated new key grant starts an independent record epoch atomically.
	source.KeyEpoch = 2
	key = bytes.Repeat([]byte{8}, 32)
	claims.Source.KeyEpoch = 2
	claims.DeliveryGeneration = 2
	claims.FenceGeneration = 2
	claims.Previous = grant.ID[:]
	replacement, err := env.SealVaultScopeKey(ctx, claims, writer, key)
	if err != nil {
		t.Fatal(err)
	}
	recipient.DeliveryGeneration = 2
	recipient.FenceGeneration = 2
	recipient.DocumentID = replacement.ID.String()
	bundle.Layers[0] = api.VaultLayerDelivery{Source: source, Recipient: recipient, RecordSequence: 1, WriterAccount: "account_1", WriterPublic: base64.RawURLEncoding.EncodeToString(replacement.Claims.WriterPublic), Envelope: base64.RawURLEncoding.EncodeToString(replacement.Raw), State: "ready"}
	mode = "rotate"
	launch("VALUE=rotated")
	bundle.Layers[0] = delivery
	if _, err := store.Environment(ctx, binding, bundle); err == nil {
		t.Fatal("revoked key epoch accepted")
	}

}

func TestLayerRecordFloorCapacityFailsClosed(t *testing.T) {
	mk := func(count int) []layerRecordFloor {
		result := make([]layerRecordFloor, count)
		for i := range result {
			id := sha256.Sum256([]byte(strconv.Itoa(i)))
			result[i] = layerRecordFloor{RecordID: hex.EncodeToString(id[:]), Revision: 1, DocumentID: env.DocumentID(id).String()}
		}
		return result
	}
	floors := map[string]layerFloor{"source": {RecordFloors: mk(4096)}}
	if err := validateRecordFloors(floors); err != nil {
		t.Fatal("supported source capacity rejected", err)
	}
	floors["source"] = layerFloor{RecordFloors: mk(4097)}
	if !errors.Is(validateRecordFloors(floors), ErrResourceExhausted) {
		t.Fatal("source record floor limit not enforced")
	}
	floors = map[string]layerFloor{}
	for i := 0; i < 4; i++ {
		floors[strconv.Itoa(i)] = layerFloor{RecordFloors: mk(4096)}
	}
	if err := validateRecordFloors(floors); err != nil {
		t.Fatal("supported total capacity rejected", err)
	}
	floors["overflow"] = layerFloor{RecordFloors: mk(1)}
	if !errors.Is(validateRecordFloors(floors), ErrResourceExhausted) {
		t.Fatal("total rollback floor limit not enforced")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeLayerState(path, make([]byte, 32), strings.Repeat("x", maximumLayerStateBytes)); !errors.Is(err, ErrResourceExhausted) {
		t.Fatal("state capacity not actionable", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("capacity failure persisted a partial state")
	}
}
