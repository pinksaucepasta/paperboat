package envinject

import (
	"context"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

// openRecords builds a complete replacement in memory. Neither partial pages nor
// plaintext enter the authenticated persistent cache.
const maximumSourceRecordFloors = 4096
const maximumRetainedRecordFloors = 16384

type layerRecordFloor struct {
	RecordID   string `json:"record_id"`
	Revision   uint64 `json:"revision"`
	Deleted    bool   `json:"deleted"`
	DocumentID string `json:"document_id"`
}

func validateRecordFloors(floors map[string]layerFloor) error {
	total := 0
	for _, source := range floors {
		if len(source.RecordFloors) > maximumSourceRecordFloors {
			return ErrResourceExhausted
		}
		total += len(source.RecordFloors)
		if total > maximumRetainedRecordFloors {
			return ErrResourceExhausted
		}
		seen := map[string]bool{}
		for _, floor := range source.RecordFloors {
			id, err := hex.DecodeString(floor.RecordID)
			if err != nil || len(id) != 32 || strings.ToLower(floor.RecordID) != floor.RecordID || floor.Revision == 0 || floor.Revision > env.MaximumContractInteger || seen[floor.RecordID] {
				return ErrInvalidSnapshot
			}
			if _, err := env.ParseDocumentID(floor.DocumentID); err != nil {
				return ErrInvalidSnapshot
			}
			seen[floor.RecordID] = true
		}
	}
	return nil
}

func (s *LayerStore) openRecords(ctx context.Context, binding LaunchContext, delivery api.VaultLayerDelivery, previous layerFloor, key []byte) (map[string][]byte, []api.VaultRecordState, []layerRecordFloor, error) {
	source := delivery.Source
	if delivery.RecordSequence > env.MaximumContractInteger || source.KeyEpoch == previous.Epoch && delivery.RecordSequence < previous.RecordSequence {
		return nil, nil, nil, ErrInvalidSnapshot
	}
	after := uint64(0)
	cached := map[string]api.VaultRecordState{}
	retained := map[string]layerRecordFloor{}
	if previous.Epoch == source.KeyEpoch {
		for _, floor := range previous.RecordFloors {
			retained[floor.RecordID] = floor
		}
	}
	if previous.Epoch == source.KeyEpoch && previous.Records != nil {
		after = previous.RecordSequence
		for _, record := range previous.Records {
			if _, exists := cached[record.RecordID]; exists {
				return nil, nil, nil, ErrInvalidSnapshot
			}
			cached[record.RecordID] = record
		}
	}
	if delivery.RecordSequence == 0 {
		cached = map[string]api.VaultRecordState{}
	} else if after != delivery.RecordSequence || previous.Records == nil || previous.Epoch != source.KeyEpoch {
		if s.config.Records == nil {
			return nil, nil, nil, ErrNotReady
		}
		changes, err := s.fetchRecords(ctx, binding, source, after, delivery.RecordSequence)
		var apiError *api.APIError
		if errors.As(err, &apiError) && apiError.Code == "record_snapshot_required" && after > 0 {
			after = 0
			cached = map[string]api.VaultRecordState{}
			changes, err = s.fetchRecords(ctx, binding, source, 0, delivery.RecordSequence)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		for _, record := range changes {
			if old, exists := retained[record.RecordID]; exists && (record.Revision < old.Revision || record.Revision == old.Revision && (record.Deleted != old.Deleted || record.DocumentID != old.DocumentID)) {
				return nil, nil, nil, ErrInvalidSnapshot
			}
			// Authenticate/decrypt tombstones too; a metadata-only deletion is insufficient.
			parsed, err := record.Decode()
			if err != nil || parsed.Claims.Issuer != s.config.Issuer {
				return nil, nil, nil, ErrInvalidSnapshot
			}
			_, value, err := env.OpenVaultRecord(ctx, parsed, key)
			clear(value)
			if err != nil {
				return nil, nil, nil, ErrInvalidSnapshot
			}
			retained[record.RecordID] = layerRecordFloor{RecordID: record.RecordID, Revision: record.Revision, Deleted: record.Deleted, DocumentID: record.DocumentID}
			if len(retained) > maximumSourceRecordFloors {
				return nil, nil, nil, ErrResourceExhausted
			}
			if record.Deleted {
				delete(cached, record.RecordID)
			} else {
				cached[record.RecordID] = record
			}
		}
	}
	if len(cached) > env.MaximumVariables {
		return nil, nil, nil, ErrInvalidSnapshot
	}
	values := map[string][]byte{}
	records := make([]api.VaultRecordState, 0, len(cached))
	cipherBytes := 0
	fail := func() (map[string][]byte, []api.VaultRecordState, []layerRecordFloor, error) {
		clearLayerValues(values)
		return nil, nil, nil, ErrInvalidSnapshot
	}
	for _, record := range cached {
		if record.WorkspaceID != source.WorkspaceID || record.OwnerKind != source.OwnerKind || record.OwnerID != source.OwnerID || record.MachineID != source.MachineID || record.KeyEpoch != source.KeyEpoch || record.Deleted || record.Sequence == 0 || record.Sequence > delivery.RecordSequence {
			return fail()
		}
		parsed, err := record.Decode()
		if err != nil || parsed.Claims.Issuer != s.config.Issuer {
			return fail()
		}
		name, value, err := env.OpenVaultRecord(ctx, parsed, key)
		if err != nil {
			return fail()
		}
		if _, exists := values[name]; exists {
			clear(value)
			return fail()
		}
		values[name] = value
		length, err := parsed.CiphertextBytes()
		if err != nil {
			return fail()
		}
		cipherBytes += length
		if cipherBytes > 256<<10 {
			return fail()
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].RecordID < records[j].RecordID })
	// A nonnil empty slice marks a verified empty cache.
	floors := make([]layerRecordFloor, 0, len(retained))
	for _, floor := range retained {
		floors = append(floors, floor)
	}
	sort.Slice(floors, func(i, j int) bool { return floors[i].RecordID < floors[j].RecordID })
	return values, records, floors, nil
}

func (s *LayerStore) fetchRecords(ctx context.Context, binding LaunchContext, source api.VaultLayerSource, after, through uint64) ([]api.VaultRecordState, error) {
	records := []api.VaultRecordState{}
	ids := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	total := 0
	for page := 0; page < 32; page++ {
		next, err := s.config.Records.VaultRecordPage(ctx, binding, source, after, through, cursor)
		if err != nil {
			return nil, err
		}
		if next.Sequence != through || next.AfterSequence != after || len(next.Records) > 200 {
			return nil, ErrInvalidSnapshot
		}
		for _, record := range next.Records {
			if ids[record.RecordID] || record.WorkspaceID != source.WorkspaceID || record.OwnerKind != source.OwnerKind || record.OwnerID != source.OwnerID || record.MachineID != source.MachineID || record.KeyEpoch != source.KeyEpoch || record.Sequence == 0 || record.Sequence > through || after > 0 && record.Sequence <= after {
				return nil, ErrInvalidSnapshot
			}
			ids[record.RecordID] = true
			total += len(record.Envelope) + len(record.WriterPublic) + len(record.DocumentID) + 512
			if len(ids) > 4096 || total > 8<<20 {
				return nil, ErrInvalidSnapshot
			}
			records = append(records, record)
		}
		if next.NextCursor == "" {
			return records, nil
		}
		if len(next.Records) == 0 || cursors[next.NextCursor] || len(next.NextCursor) > 4096 {
			return nil, ErrInvalidSnapshot
		}
		cursors[next.NextCursor] = true
		cursor = next.NextCursor
	}
	return nil, ErrInvalidSnapshot
}
