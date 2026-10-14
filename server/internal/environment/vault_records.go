package environment

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

const vaultRecordCursorLifetime = 10 * time.Minute
const recordScopePredicate = `workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4 AND key_epoch=$5`

func recordArgs(c VaultLayerCoordinate, epoch uint64) []any {
	return []any{c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, epoch}
}

type vaultRecordHead struct {
	Epoch, Sequence, Floor uint64
	CursorKey              []byte
}

// Epoch replacement must commit staged records with the scope rotation. Reads
// never silently discard old ciphertext when a rotation has not completed.
func ensureVaultRecordHeadTx(ctx context.Context, tx *db.Tx, c VaultLayerCoordinate, epoch uint64) (vaultRecordHead, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return vaultRecordHead{}, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO environment_vault_record_heads(workspace_id,owner_kind,owner_id,machine_id,key_epoch,cursor_key) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, epoch, key)
	if err != nil {
		return vaultRecordHead{}, err
	}
	var h vaultRecordHead
	err = tx.QueryRow(ctx, `SELECT key_epoch,sequence,floor_sequence,cursor_key FROM environment_vault_record_heads WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4 FOR UPDATE`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID).Scan(&h.Epoch, &h.Sequence, &h.Floor, &h.CursorKey)
	if err == nil && h.Epoch != epoch {
		return h, ErrVaultRotationRequired
	}
	return h, err
}
func authorizeVaultRecordsTx(ctx context.Context, tx *db.Tx, account string, c VaultLayerCoordinate, write bool) (uint64, error) {
	epoch, err := authorizeVaultScopeWorkspaceTx(ctx, tx, account, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID)
	if err != nil {
		return 0, err
	}
	if write {
		allowed, err := vaultLayerWriterTx(ctx, tx, account, c)
		if err != nil {
			return 0, err
		}
		if !allowed {
			return 0, ErrKeyAuthorizationRequired
		}
	}
	scope, err := readVaultScopeWorkspaceTx(ctx, tx, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID)
	if err != nil {
		return 0, err
	}
	if scope.KeyEpoch != epoch {
		return 0, ErrVaultRotationRequired
	}
	return epoch, nil
}
func (s *Service) PutVaultRecords(ctx context.Context, account string, c VaultLayerCoordinate, in VaultRecordsPut) (VaultRecordsPage, error) {
	out := VaultRecordsPage{Records: []VaultRecordState{}}
	if !validLayerCoordinate(c) || len(in.Records) == 0 || len(in.Records) > MaximumVaultRecordBatch {
		return out, ErrProtocolInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if err := allowVaultWritesTx(ctx, tx, account); err != nil {
			return err
		}
		epoch, err := authorizeVaultRecordsTx(ctx, tx, account, c, true)
		if err != nil {
			return err
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, in.OperationID, "records:"+c.WorkspaceID+":"+c.OwnerKind+":"+c.OwnerID+":"+c.MachineID, in, &out)
		if err != nil || replay {
			return err
		}
		writer, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		head, err := ensureVaultRecordHeadTx(ctx, tx, c, epoch)
		if err != nil {
			return err
		}
		if head.Sequence >= MaxBrowserInteger {
			return ErrLimitExceeded
		}
		sequence := head.Sequence + 1
		seen := map[string]bool{}
		total := 0
		for _, mutation := range in.Records {
			raw, err := DecodeCanonicalBase64URL(mutation.Envelope, MaximumVaultRecordBytes)
			if err != nil {
				return err
			}
			total += len(raw)
			if total > (256<<10)+MaximumVaultRecordBatch*2048 {
				return ErrLimitExceeded
			}
			r, err := parseVaultRecord(raw, s.passwordVaultIssuer, writer)
			if err != nil {
				return err
			}
			if (VaultLayerCoordinate{WorkspaceID: r.Claims.WorkspaceID, OwnerKind: r.Claims.OwnerKind, OwnerID: r.Claims.OwnerID, MachineID: r.Claims.MachineID}) != c || r.Claims.KeyEpoch != epoch {
				return ErrKeyAuthorizationRequired
			}
			if seen[r.Claims.RecordID] || mutation.ExpectedRevision >= MaxBrowserInteger || r.Claims.Revision != mutation.ExpectedRevision+1 {
				return ErrProtocolInvalid
			}
			seen[r.Claims.RecordID] = true
			var revision uint64
			err = tx.QueryRow(ctx, `SELECT revision FROM environment_vault_record_versions WHERE `+recordScopePredicate+` AND record_id=$6 ORDER BY revision DESC LIMIT 1`, append(recordArgs(c, epoch), r.Claims.RecordID)...).Scan(&revision)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if revision != mutation.ExpectedRevision {
				return ErrVersionConflict
			}
			state := recordState(c, r, sequence, writer.WriterPublicKey[:])
			args := append(recordArgs(c, epoch), state.RecordID, state.Revision, state.Deleted, sequence, state.DocumentID, raw, r.CiphertextBytes, writer.WriterPublicKey[:])
			_, err = tx.Exec(ctx, `INSERT INTO environment_vault_record_versions(workspace_id,owner_kind,owner_id,machine_id,key_epoch,record_id,revision,deleted,sequence,document_id,envelope,ciphertext_bytes,writer_public) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, args...)
			if err != nil {
				return err
			}
			out.Records = append(out.Records, state)
		}
		var count, live, bytes int
		err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE NOT deleted),COALESCE(sum(ciphertext_bytes) FILTER(WHERE NOT deleted),0) FROM(SELECT DISTINCT ON(record_id) deleted,ciphertext_bytes FROM environment_vault_record_versions WHERE `+recordScopePredicate+` ORDER BY record_id,revision DESC) current`, recordArgs(c, epoch)...).Scan(&count, &live, &bytes)
		if err != nil {
			return err
		}
		if count > MaximumVaultRecordIDs || live > 128 || bytes > MaximumVaultRecordLiveBytes {
			return ErrLimitExceeded
		}
		_, err = tx.Exec(ctx, `UPDATE environment_vault_record_heads SET sequence=$5 WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, sequence)
		if err != nil {
			return err
		}
		out.Sequence = sequence
		out.AfterSequence = head.Sequence
		if err := pruneVaultRecordHistoryTx(ctx, tx, c, epoch, sequence, s.now().Add(-vaultRecordCursorLifetime)); err != nil {
			return err
		}
		return saveVaultOperationTx(ctx, tx, account, in.OperationID, digest, out)
	})
	return out, err
}
func pruneVaultRecordHistoryTx(ctx context.Context, tx *db.Tx, c VaultLayerCoordinate, epoch, sequence uint64, cutoff time.Time) error {
	args := append(recordArgs(c, epoch), cutoff)
	var floor uint64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(sequence),0) FROM environment_vault_record_versions WHERE `+recordScopePredicate+` AND created_at<$6`, args...).Scan(&floor); err != nil {
		return err
	}
	prune := func() error {
		if floor == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `DELETE FROM environment_vault_record_versions old WHERE `+`old.workspace_id=$1 AND old.owner_kind=$2 AND old.owner_id=$3 AND old.machine_id=$4 AND old.key_epoch=$5 AND old.sequence<$6 AND EXISTS(SELECT 1 FROM environment_vault_record_versions newer WHERE newer.workspace_id=old.workspace_id AND newer.owner_kind=old.owner_kind AND newer.owner_id=old.owner_id AND newer.machine_id=old.machine_id AND newer.key_epoch=old.key_epoch AND newer.record_id=old.record_id AND newer.sequence>old.sequence AND newer.sequence<=$6)`, append(recordArgs(c, epoch), floor)...)
		return err
	}
	if err := prune(); err != nil {
		return err
	}
	for {
		var size int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(envelope)),0) FROM environment_vault_record_versions WHERE `+recordScopePredicate, recordArgs(c, epoch)...).Scan(&size); err != nil {
			return err
		}
		if size <= MaximumVaultRecordHistoryBytes {
			break
		}
		var next uint64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(min(sequence),0) FROM environment_vault_record_versions WHERE `+recordScopePredicate+` AND sequence>$6`, append(recordArgs(c, epoch), floor)...).Scan(&next); err != nil {
			return err
		}
		if next == 0 || next > sequence {
			return ErrLimitExceeded
		}
		floor = next
		if err := prune(); err != nil {
			return err
		}
	}
	if floor > 0 {
		_, err := tx.Exec(ctx, `UPDATE environment_vault_record_heads SET floor_sequence=GREATEST(floor_sequence,$5) WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, floor)
		return err
	}
	return nil
}

type vaultRecordCursor struct {
	Coordinate            VaultLayerCoordinate
	Account               string
	Epoch, After, Through uint64
	Last                  string
	Expires               int64
}

func encodeRecordCursor(c vaultRecordCursor, key []byte) string {
	b, _ := json.Marshal(c)
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(append(b, m.Sum(nil)...))
}
func decodeRecordCursor(token string, key []byte) (vaultRecordCursor, error) {
	var c vaultRecordCursor
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) <= 32 || len(b) > 2048 {
		return c, ErrProtocolInvalid
	}
	raw, sig := b[:len(b)-32], b[len(b)-32:]
	m := hmac.New(sha256.New, key)
	m.Write(raw)
	if !hmac.Equal(sig, m.Sum(nil)) || json.Unmarshal(raw, &c) != nil {
		return c, ErrProtocolInvalid
	}
	return c, nil
}
func readVaultRecordsPageTx(ctx context.Context, tx *db.Tx, account string, c VaultLayerCoordinate, epoch uint64, q VaultRecordsQuery, now time.Time) (VaultRecordsPage, error) {
	out := VaultRecordsPage{Records: []VaultRecordState{}, AfterSequence: q.AfterSequence}
	if q.Limit < 1 || q.Limit > 200 || q.AfterSequence > MaxBrowserInteger {
		return out, ErrProtocolInvalid
	}
	head, err := ensureVaultRecordHeadTx(ctx, tx, c, epoch)
	if err != nil {
		return out, err
	}
	through := head.Sequence
	if q.ThroughSequence != nil {
		through = *q.ThroughSequence
	}
	last := ""
	expires := now.Add(vaultRecordCursorLifetime).Unix()
	if q.Cursor != "" {
		cur, err := decodeRecordCursor(q.Cursor, head.CursorKey)
		if err != nil {
			return out, err
		}
		if cur.Coordinate != c || cur.Account != account || cur.Epoch != epoch || cur.After != q.AfterSequence || (q.ThroughSequence != nil && cur.Through != *q.ThroughSequence) {
			return out, ErrKeyAuthorizationRequired
		}
		if cur.Expires <= now.Unix() {
			return out, ErrVaultRecordCursorExpired
		}
		through, last, expires = cur.Through, cur.Last, cur.Expires
	}
	if through > head.Sequence || q.AfterSequence > through {
		return out, ErrProtocolInvalid
	}
	if through < head.Floor || (q.AfterSequence > 0 && q.AfterSequence < head.Floor) {
		return out, ErrVaultRecordCursorExpired
	}
	out.Sequence = through
	args := append(recordArgs(c, epoch), through, q.AfterSequence, last, q.Limit+1)
	rows, err := tx.Query(ctx, `SELECT record_id,revision,deleted,sequence,document_id,envelope,writer_public FROM(SELECT DISTINCT ON(record_id) record_id,revision,deleted,sequence,document_id,envelope,writer_public FROM environment_vault_record_versions WHERE `+recordScopePredicate+` AND sequence<=$6 ORDER BY record_id,revision DESC) snapshot WHERE ($7=0 OR sequence>$7) AND record_id>$8 ORDER BY record_id LIMIT $9`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		state := VaultRecordState{VaultLayerCoordinate: c, KeyEpoch: epoch}
		var raw, writer []byte
		if err := rows.Scan(&state.RecordID, &state.Revision, &state.Deleted, &state.Sequence, &state.DocumentID, &raw, &writer); err != nil {
			return out, err
		}
		state.Envelope = base64.RawURLEncoding.EncodeToString(raw)
		state.WriterPublic = base64.RawURLEncoding.EncodeToString(writer)
		out.Records = append(out.Records, state)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Records) > q.Limit {
		out.Records = out.Records[:q.Limit]
		out.NextCursor = encodeRecordCursor(vaultRecordCursor{Coordinate: c, Account: account, Epoch: epoch, After: q.AfterSequence, Through: through, Last: out.Records[len(out.Records)-1].RecordID, Expires: expires}, head.CursorKey)
	}
	return out, nil
}
func (s *Service) ListVaultRecords(ctx context.Context, account string, c VaultLayerCoordinate, q VaultRecordsQuery) (VaultRecordsPage, error) {
	var out VaultRecordsPage
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		epoch, err := authorizeVaultRecordsTx(ctx, tx, account, c, false)
		if err != nil {
			return err
		}
		out, err = readVaultRecordsPageTx(ctx, tx, account, c, epoch, q, s.now())
		return err
	})
	return out, err
}
func (s *Service) GetVaultRecord(ctx context.Context, account string, c VaultLayerCoordinate, id string) (VaultRecordState, error) {
	var out VaultRecordState
	if !validVaultRecordID(id) {
		return out, ErrProtocolInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		epoch, err := authorizeVaultRecordsTx(ctx, tx, account, c, false)
		if err != nil {
			return err
		}
		out = VaultRecordState{VaultLayerCoordinate: c, KeyEpoch: epoch}
		var raw, writer []byte
		err = tx.QueryRow(ctx, `SELECT record_id,revision,deleted,sequence,document_id,envelope,writer_public FROM environment_vault_record_versions WHERE `+recordScopePredicate+` AND record_id=$6 ORDER BY revision DESC LIMIT 1`, append(recordArgs(c, epoch), id)...).Scan(&out.RecordID, &out.Revision, &out.Deleted, &out.Sequence, &out.DocumentID, &raw, &writer)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		out.Envelope = base64.RawURLEncoding.EncodeToString(raw)
		out.WriterPublic = base64.RawURLEncoding.EncodeToString(writer)
		return err
	})
	return out, err
}

// Machine records are selected from the same fresh authorized launch context;
// callers cannot name another member's private coordinate.
func (s *Service) ListVaultHostRecords(ctx context.Context, machineOwner, machine, actor, workspace string, c VaultLayerCoordinate, q VaultRecordsQuery) (VaultRecordsPage, error) {
	var out VaultRecordsPage
	launch, err := s.VaultLayerContext(ctx, machineOwner, machine, actor, workspace)
	if err != nil {
		return out, err
	}
	var layer *VaultLayerDelivery
	for i := range launch.Layers {
		if launch.Layers[i].Source.VaultLayerCoordinate == c {
			layer = &launch.Layers[i]
			break
		}
	}
	if layer == nil {
		return out, ErrKeyAuthorizationRequired
	}
	if layer.State != "ready" {
		return out, ErrVaultRotationRequired
	}
	// Reauthorize and read under one scope lock so revocation between context and
	// record read cannot disclose another actor's private source.
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if _, err := authorizeVaultHostTx(ctx, tx, machineOwner, machine); err != nil {
			return err
		}
		if workspace != "personal" {
			if err := requireVaultWorkspaceMemberTx(ctx, tx, actor, workspace); err != nil {
				return err
			}
			if err := requireVaultWorkspaceMemberTx(ctx, tx, machineOwner, workspace); err != nil {
				return err
			}
		}
		current, err := readVaultLayerTx(ctx, tx, machine, c)
		if err != nil {
			return err
		}
		if current.State != "ready" || current.Source.KeyEpoch != layer.Source.KeyEpoch {
			return ErrVaultRotationRequired
		}
		out, err = readVaultRecordsPageTx(ctx, tx, actor, c, current.Source.KeyEpoch, q, s.now())
		return err
	})
	return out, err
}
