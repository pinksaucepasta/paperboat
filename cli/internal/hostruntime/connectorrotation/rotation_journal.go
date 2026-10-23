package connectorrotation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MemoryJournal is useful for deterministic unit tests and embedders that
// already provide a durable transaction around this package.
type MemoryJournal struct {
	mu      sync.Mutex
	records map[string]Record
}

func NewMemoryJournal() *MemoryJournal { return &MemoryJournal{records: make(map[string]Record)} }

func (j *MemoryJournal) Load(ctx context.Context, operationID string) (Record, error) {
	if j == nil || ctx == nil {
		return Record{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[operationID]
	if !ok {
		return Record{}, ErrOperationNotFound
	}
	return record.clone(), nil
}

func (j *MemoryJournal) Save(ctx context.Context, record Record) error {
	if j == nil || ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Validate(time.Time{}); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if existing, ok := j.records[record.OperationID]; ok && !sameStableRecord(existing, record) {
		return ErrOperationConflict
	}
	j.records[record.OperationID] = record.clone()
	return nil
}

func sameStableRecord(a, b Record) bool {
	return a.Version == b.Version && a.AccountID == b.AccountID && a.TunnelID == b.TunnelID && a.OperationID == b.OperationID && a.ConnectorID == b.ConnectorID && a.HostID == b.HostID && a.TargetSetHash == b.TargetSetHash && a.OldCredentialReference == b.OldCredentialReference && a.OldIdentityKeyID == b.OldIdentityKeyID && a.OldIdentityThumbprint == b.OldIdentityThumbprint && a.OldCredentialGeneration == b.OldCredentialGeneration && a.NewCredentialGeneration == b.NewCredentialGeneration && a.NewKey.Reference == b.NewKey.Reference && a.NewKey.KeyID == b.NewKey.KeyID && a.NewKey.Thumbprint == b.NewKey.Thumbprint && equalBytes(a.NewKey.PublicKey, b.NewKey.PublicKey)
}

// FileJournal provides a small atomic, 0600 metadata journal. Its checksum and
// last-known-good sidecar make corruption fail closed while still allowing a
// restart to recover the last committed operation phase. It is intentionally
// separate from hoststate.State so in-flight operations cannot be mistaken for
// desired tunnel state.
type FileJournal struct {
	mu          sync.Mutex
	path        string
	records     map[string]Record
	needsRepair bool
}

type fileDocument struct {
	Version  int      `json:"version"`
	Records  []Record `json:"records"`
	Checksum string   `json:"checksum"`
}

type fileDocumentPayload struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

func OpenFileJournal(path string) (*FileJournal, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(path) || clean != path || path == string(filepath.Separator) {
		return nil, ErrInvalidConfig
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateJournalDirectory(parent); err != nil {
		return nil, safeRotationFailure("connector rotation journal directory is unavailable", err)
	}
	journal := &FileJournal{path: path, records: make(map[string]Record)}
	records, exists, err := readJournalRecords(path)
	if err == nil && exists {
		journal.records = records
		return journal, nil
	}
	if err == nil && !exists {
		backupRecords, backupExists, backupErr := readJournalRecords(journal.backupPath())
		if backupErr != nil {
			return nil, backupErr
		}
		if backupExists {
			journal.records = backupRecords
			journal.needsRepair = true
		}
		return journal, nil
	}
	if journalRecoveryEligible(err) {
		backupRecords, backupExists, backupErr := readJournalRecords(journal.backupPath())
		if backupErr != nil {
			return nil, safeRotationFailure("connector rotation journal recovery could not be verified", errors.Join(err, backupErr))
		}
		if !backupExists {
			return nil, safeRotationFailure("connector rotation journal is corrupt and has no verified recovery copy", err)
		}
		journal.records = backupRecords
		journal.needsRepair = true
		return journal, nil
	}
	return nil, safeRotationFailure("connector rotation journal could not be read safely", err)
}

func (j *FileJournal) Load(ctx context.Context, operationID string) (Record, error) {
	if j == nil || ctx == nil {
		return Record{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	record, ok := j.records[operationID]
	if !ok {
		return Record{}, ErrOperationNotFound
	}
	return record.clone(), nil
}

func (j *FileJournal) Save(ctx context.Context, record Record) error {
	if j == nil || ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := record.Validate(time.Time{}); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if existing, ok := j.records[record.OperationID]; ok && !sameStableRecord(existing, record) {
		return ErrOperationConflict
	}
	next := make(map[string]Record, len(j.records)+1)
	for id, value := range j.records {
		next[id] = value
	}
	next[record.OperationID] = record.clone()
	pruneFinishedRecords(next, record.OperationID)
	if len(next) > maxJournalRecords {
		return ErrJournalUncertain
	}
	ids := make([]string, 0, len(next))
	for id := range next {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	document := fileDocumentPayload{Version: journalVersion, Records: make([]Record, 0, len(ids))}
	for _, id := range ids {
		document.Records = append(document.Records, next[id])
	}
	encoded, err := marshalJournalDocument(document)
	if err != nil {
		return err
	}
	if len(encoded) > maxJournalBytes {
		return ErrJournalUncertain
	}
	if !j.needsRepair {
		previous, readErr := readPrivateJournalFile(j.path, maxJournalBytes)
		if readErr == nil {
			if _, decodeErr := decodeJournalRecords(previous); decodeErr != nil {
				return safeRotationFailure("connector rotation journal contains invalid data", errors.Join(ErrJournalUncertain, decodeErr))
			}
			if err := writePrivateJournalFile(j.backupPath(), previous); err != nil {
				return journalOperationFailure(err)
			}
		} else if !errorLeavesMatch(readErr, errJournalFileNotExist) {
			return safeRotationFailure("connector rotation journal could not be read before saving", errors.Join(ErrJournalUncertain, readErr))
		}
	}
	if err := writePrivateJournalFile(j.path, encoded); err != nil {
		return journalOperationFailure(err)
	}
	j.records = next
	j.needsRepair = false
	return nil
}

func (j *FileJournal) backupPath() string { return j.path + ".bak" }

func marshalJournalDocument(payload fileDocumentPayload) ([]byte, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payloadBytes)
	return json.Marshal(fileDocument{
		Version:  payload.Version,
		Records:  payload.Records,
		Checksum: "sha256:" + hex.EncodeToString(digest[:]),
	})
}

func readJournalRecords(path string) (map[string]Record, bool, error) {
	data, err := readPrivateJournalFile(path, maxJournalBytes)
	if errorLeavesMatch(err, errJournalFileNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, ErrJournalCorrupt
	}
	records, err := decodeJournalRecords(data)
	return records, true, err
}

func decodeJournalRecords(data []byte) (map[string]Record, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, ErrJournalCorrupt
	}
	var document fileDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || document.Version != journalVersion || len(document.Records) > maxJournalRecords || !validSHA256Hash(document.Checksum) {
		return nil, ErrJournalCorrupt
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, ErrJournalCorrupt
	}
	payload, err := json.Marshal(fileDocumentPayload{Version: document.Version, Records: document.Records})
	if err != nil {
		return nil, ErrJournalCorrupt
	}
	digest := sha256.Sum256(payload)
	if document.Checksum != "sha256:"+hex.EncodeToString(digest[:]) {
		return nil, ErrJournalCorrupt
	}
	records := make(map[string]Record, len(document.Records))
	for _, record := range document.Records {
		if record.Validate(time.Time{}) != nil {
			return nil, ErrJournalCorrupt
		}
		if _, exists := records[record.OperationID]; exists {
			return nil, ErrJournalCorrupt
		}
		records[record.OperationID] = record.clone()
	}
	return records, nil
}

var _ Journal = (*MemoryJournal)(nil)

var _ Journal = (*FileJournal)(nil)

// rejectDuplicateJSONKeys catches duplicate object members before the strict
// document decoder runs. encoding/json intentionally accepts the last value
// for a duplicate key, which would make a hand-edited or partially corrupted
// recovery journal ambiguous.
func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ErrJournalCorrupt
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return ErrJournalCorrupt
			}
			if _, duplicate := seen[name]; duplicate {
				return ErrJournalCorrupt
			}
			seen[name] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return ErrJournalCorrupt
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return ErrJournalCorrupt
		}
	default:
		return ErrJournalCorrupt
	}
	return nil
}

// pruneFinishedRecords drops the oldest finished rotations (old credential revoked) until the journal is
// within maxJournalRecords; keep is the record being saved. Without it every connector stopped rotating
// after 64 rotations (Lane X audit H3). Only a journal full of unfinished rotations stays over the cap.
func pruneFinishedRecords(records map[string]Record, keep string) {
	if len(records) <= maxJournalRecords {
		return
	}
	finished := make([]Record, 0, len(records))
	for id, record := range records {
		if id != keep && record.Phase == PhaseRevoked {
			finished = append(finished, record)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		if !finished[i].UpdatedAt.Equal(finished[j].UpdatedAt) {
			return finished[i].UpdatedAt.Before(finished[j].UpdatedAt)
		}
		return finished[i].OperationID < finished[j].OperationID
	})
	for _, record := range finished {
		if len(records) <= maxJournalRecords {
			return
		}
		delete(records, record.OperationID)
	}
}
