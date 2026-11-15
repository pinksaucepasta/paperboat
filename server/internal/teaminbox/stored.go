package teaminbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teambucket"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	bucket "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

type Buckets interface {
	StoreFor(context.Context, *db.Tx, string) (teambucket.ObjectStore, error)
}

func (s *Service) SetBuckets(b Buckets) { s.buckets = b }

func (s *Service) storedBucket(ctx context.Context, tx *db.Tx, team string) (teambucket.ObjectStore, error) {
	if s.buckets == nil {
		return nil, ErrOffline
	}
	store, err := s.buckets.StoreFor(ctx, tx, team)
	if errors.Is(err, teambucket.ErrNotFound) {
		return nil, ErrOffline
	}
	return store, err
}

func storedRecipientsTx(ctx context.Context, tx *db.Tx, account string) ([]wire.Recipient, error) {
	rows, err := tx.Query(ctx, `SELECT h.machine_id,h.host_public FROM environment_vault_hosts h
 JOIN user_machines m ON m.id=h.machine_id AND m.user_id=h.account_id
 WHERE h.account_id=$1 AND m.deleted_at IS NULL AND m.revoked_at IS NULL
 AND m.installation_generation=h.installation_generation
 AND m.configured_capabilities @> ARRAY['file_receive'] ORDER BY h.machine_id LIMIT $2`, account, wire.MaxRecipients+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.Recipient{}
	for rows.Next() {
		var r wire.Recipient
		if err := rows.Scan(&r.MachineID, &r.HostPublic); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, ErrOffline
	}
	if len(out) > wire.MaxRecipients {
		return nil, ErrLimit
	}
	return out, rows.Err()
}

func (s *Service) StoredRecipients(ctx context.Context, sender, id string) ([]wire.Recipient, error) {
	var out []wire.Recipient
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		r, err := readTx(ctx, tx, id, sender)
		if err != nil {
			return err
		}
		if !r.Stored {
			return ErrNotFound
		}
		if r.RecipientAccount == sender {
			m, _, err := readStoredTx(ctx, tx, id)
			if err != nil {
				return err
			}
			for machine := range m.SealedKeys {
				out = append(out, wire.Recipient{MachineID: machine})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].MachineID < out[j].MachineID })
			return nil
		}
		if r.SenderAccount != sender {
			return ErrNotFound
		}
		out, err = storedRecipientsTx(ctx, tx, r.RecipientAccount)
		return err
	})
	return out, err
}

// PutStored freezes the ciphertext manifest before any upload URL is issued. A
// retry must present exactly the same manifest, including every sealed key.
func (s *Service) PutStored(ctx context.Context, sender, id string, in wire.Metadata) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		r, err := readTx(ctx, tx, id, sender)
		if err != nil {
			return err
		}
		if r.SenderAccount != sender || !r.Stored {
			return ErrNotFound
		}
		if !r.ExpiresAt.After(s.now()) || (r.Status != "pending" && r.Status != "approved") {
			return ErrConflict
		}
		recipients, err := storedRecipientsTx(ctx, tx, r.RecipientAccount)
		if err != nil {
			return err
		}
		if err = validateStored(r.Files, in, recipients); err != nil {
			return err
		}
		var public string
		if err = tx.QueryRow(ctx, `SELECT public_identity_key FROM user_machines WHERE id=$1 AND revoked_at IS NULL AND deleted_at IS NULL`, r.SourceMachineID).Scan(&public); err != nil {
			return err
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(public)
		if err != nil || !bytes.Equal(decoded, in.WriterPublic) {
			return ErrForbidden
		}
		raw, err := json.Marshal(in)
		if err != nil || len(raw) > wire.MaxMetadataBytes {
			return ErrInvalid
		}
		var existing []byte
		if err = tx.QueryRow(ctx, `SELECT stored_metadata FROM team_inbox_requests WHERE request_id=$1 FOR UPDATE`, id).Scan(&existing); err != nil {
			return err
		}
		if len(existing) > 0 {
			var old wire.Metadata
			if json.Unmarshal(existing, &old) != nil || !reflect.DeepEqual(old, in) {
				return ErrConflict
			}
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE team_inbox_requests SET stored_metadata=$2,updated_at=$3 WHERE request_id=$1`, id, raw, s.now())
		return err
	})
}

func validateStored(files []File, in wire.Metadata, recipients []wire.Recipient) error {
	if len(in.WriterPublic) != 32 || len(in.Parts) < 1 || len(in.Parts) > wire.MaxParts || len(in.SealedKeys) != len(recipients) {
		return ErrInvalid
	}
	for _, r := range recipients {
		if len(in.SealedKeys[r.MachineID]) < 32 || len(in.SealedKeys[r.MachineID]) > 2048 {
			return ErrInvalid
		}
	}
	offsets := make([]int64, len(files))
	seen := make([]bool, len(files))
	last := 0
	for i, p := range in.Parts {
		if p.Index != i || p.File < 0 || p.File < last || p.File >= len(files) || p.Offset != offsets[p.File] || p.Bytes < 28 || p.Bytes > bucket.MaxObjectBytes {
			return ErrInvalid
		}
		if (bucket.Segment{Index: i, Bytes: p.Bytes, SHA256: p.SHA256}).Validate() != nil {
			return ErrInvalid
		}
		offsets[p.File] += p.Bytes - 28
		seen[p.File] = true
		last = p.File
	}
	for i, f := range files {
		if !seen[i] || offsets[i] != f.Size {
			return ErrInvalid
		}
	}
	return nil
}

func readStoredTx(ctx context.Context, tx *db.Tx, id string) (wire.Metadata, string, error) {
	var raw []byte
	var prefix string
	err := tx.QueryRow(ctx, `SELECT stored_metadata,stored_prefix FROM team_inbox_requests WHERE request_id=$1 AND stored_prefix IS NOT NULL AND stored_deleted_at IS NULL`, id).Scan(&raw, &prefix)
	if noRows(err) {
		return wire.Metadata{}, "", ErrNotFound
	}
	if err != nil {
		return wire.Metadata{}, "", err
	}
	var m wire.Metadata
	if len(raw) == 0 {
		return m, "", ErrPending
	}
	err = json.Unmarshal(raw, &m)
	return m, prefix, err
}

// StoredUploadURLs binds each PUT to the frozen size and checksum. The expiry
// is retained so cleanup also removes a late PUT made with an outstanding URL.
func (s *Service) StoredUploadURLs(ctx context.Context, sender, id string, in wire.Range) (wire.URLs, error) {
	var out wire.URLs
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		r, err := readTx(ctx, tx, id, sender)
		if err != nil {
			return err
		}
		if r.SenderAccount != sender || !r.Stored {
			return ErrNotFound
		}
		if !r.ExpiresAt.After(s.now()) || (r.Status != "pending" && r.Status != "approved") {
			return ErrConflict
		}
		m, prefix, err := readStoredTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if in.First < 0 || in.Count < 1 || in.Count > bucket.MaxSegmentsPerBatch || in.First > len(m.Parts)-in.Count {
			return ErrInvalid
		}
		store, err := s.storedBucket(ctx, tx, r.TeamID)
		if err != nil {
			return err
		}
		if !store.Config().UseFiles || store.Config().Prefix != prefix {
			return ErrConflict
		}
		for _, p := range m.Parts[in.First : in.First+in.Count] {
			key, err := wire.PartKey(prefix, id, p.Index)
			if err != nil {
				return ErrInvalid
			}
			authority, err := store.PresignPut(ctx, key, p.Bytes, p.SHA256, bucket.URLLifetime)
			if err != nil {
				return err
			}
			out.URLs = append(out.URLs, authority)
		}
		_, err = tx.Exec(ctx, `UPDATE team_inbox_requests SET stored_upload_until=$2 WHERE request_id=$1`, id, s.now().Add(bucket.URLLifetime))
		return err
	})
	return out, err
}

func (s *Service) StoredUploaded(ctx context.Context, sender, id string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE team_inbox_requests SET stored_uploaded=true,updated_at=$3
 WHERE request_id=$1 AND sender_account=$2 AND stored_metadata IS NOT NULL AND stored_deleted_at IS NULL
 AND expires_at>$3 AND status IN('pending','approved')`, id, sender, s.now())
		if err == nil && tag.RowsAffected() == 0 {
			return ErrConflict
		}
		return err
	})
}

// storedAuthorized is independent of sender reachability. Current membership,
// machine custody and the original file grant remain required after upload.
const storedAuthorized = `EXISTS(SELECT 1 FROM teams t
 JOIN team_members a ON a.team_id=t.team_id AND a.account_id=r.sender_account AND a.active
 JOIN team_members b ON b.team_id=t.team_id AND b.account_id=r.recipient_account AND b.active
 JOIN user_machines source ON source.id=r.source_machine_id AND source.revoked_at IS NULL AND source.deleted_at IS NULL
 JOIN user_machines d ON d.id=r.destination_machine_id AND d.user_id=r.recipient_account AND d.deleted_at IS NULL AND d.revoked_at IS NULL
 WHERE t.team_id=r.team_id AND t.deleted_at IS NULL AND NOT t.suspended AND team_paid_entitled(t.team_id)
 AND team_machine_capability_allowed(r.sender_account,r.team_id,r.destination_machine_id,'files'))`

const storedMachineAllowed = `EXISTS(SELECT 1 FROM user_machines m JOIN environment_vault_hosts h ON h.machine_id=m.id AND h.account_id=m.user_id
 WHERE m.id=$1 AND m.user_id=r.recipient_account AND m.deleted_at IS NULL AND m.revoked_at IS NULL
 AND m.installation_generation=h.installation_generation AND m.configured_capabilities @> ARRAY['file_receive'])
 AND coalesce(r.stored_metadata->'sealed_keys' ? $1,false)`

func (s *Service) StoredDeliverable(ctx context.Context, machine string) (wire.Page, error) {
	out := wire.Page{Transfers: []wire.Delivery{}}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.request_id,r.manifest_digest,r.manifest,r.stored_metadata,r.source_machine_id,r.sender_account FROM team_inbox_requests r
 WHERE r.stored_uploaded AND r.stored_deleted_at IS NULL AND r.expires_at>$2
 AND (r.status='approved' AND r.stored_machine_id=$1 OR r.status='consumed' AND r.stored_machine_id=$1)
 AND `+storedMachineAllowed+` AND `+storedAuthorized+` ORDER BY r.created_at LIMIT 8`, machine, s.now())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d wire.Delivery
			var files, meta []byte
			if err := rows.Scan(&d.RequestID, &d.ManifestDigest, &files, &meta, &d.SourceMachineID, &d.SenderAccount); err != nil {
				return err
			}
			if err := json.Unmarshal(files, &d.Files); err != nil {
				return err
			}
			if err := json.Unmarshal(meta, &d.Metadata); err != nil {
				return err
			}
			d.Metadata.SealedKeys = map[string][]byte{machine: d.Metadata.SealedKeys[machine]}
			out.Transfers = append(out.Transfers, d)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) StoredDownloadURLs(ctx context.Context, machine, id string, in wire.Range) (wire.URLs, error) {
	var out wire.URLs
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		// The first recipient machine claims the transfer atomically. Every retry is
		// fenced to that same machine, so two online hosts cannot both publish it.
		var team string
		err := tx.QueryRow(ctx, `UPDATE team_inbox_requests r SET stored_machine_id=$1,status='consumed',consumed_at=coalesce(consumed_at,$3),updated_at=$3
 WHERE r.request_id=$2 AND r.stored_uploaded AND r.stored_deleted_at IS NULL AND r.expires_at>$3
 AND (r.status='approved' AND r.stored_machine_id=$1 OR r.status='consumed' AND r.stored_machine_id=$1)
 AND `+storedMachineAllowed+` AND `+storedAuthorized+` RETURNING r.team_id`, machine, id, s.now()).Scan(&team)
		if noRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m, prefix, err := readStoredTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if in.First < 0 || in.Count < 1 || in.Count > bucket.MaxSegmentsPerBatch || in.First > len(m.Parts)-in.Count {
			return ErrInvalid
		}
		store, err := s.storedBucket(ctx, tx, team)
		if err != nil {
			return err
		}
		for _, p := range m.Parts[in.First : in.First+in.Count] {
			key, err := wire.PartKey(prefix, id, p.Index)
			if err != nil {
				return ErrInvalid
			}
			a, err := store.PresignGet(ctx, key, bucket.URLLifetime)
			if err != nil {
				return err
			}
			out.URLs = append(out.URLs, a)
		}
		return nil
	})
	return out, err
}

func (s *Service) StoredReceived(ctx context.Context, machine, id, digest string) error {
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE team_inbox_requests r SET status='completed',completed_at=coalesce(completed_at,$4),updated_at=$4,
 decision_generation=decision_generation+CASE WHEN status='consumed' THEN 1 ELSE 0 END,
 committed_bytes=(SELECT sum((f->>'size')::bigint) FROM jsonb_array_elements(r.manifest) f),transfer_state='done',pause_reason=NULL
 WHERE r.request_id=$2 AND r.stored_machine_id=$1 AND r.manifest_digest=$3 AND r.status IN('consumed','completed')
 AND `+storedMachineAllowed+` AND `+storedAuthorized, machine, id, digest, s.now())
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		r, err := readTx(ctx, tx, id, "")
		if err != nil {
			return err
		}
		if err := s.queueReceiptTx(ctx, tx, r.RecipientAccount, id); err != nil {
			return err
		}
		return auditLifecycleTx(ctx, tx, r.RecipientAccount, r, "complete", id, "completed")
	})
	if err != nil {
		return err
	}
	return s.DeleteStored(ctx, id)
}

// DeleteStored is retryable: keep the manifest until every delete succeeds and
// all outstanding PUT authorities expire. No object body or file key is read.
func (s *Service) DeleteStored(ctx context.Context, id string) error {
	var m wire.Metadata
	var prefix, team string
	var until *time.Time
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT stored_metadata,stored_prefix,team_id,stored_upload_until FROM team_inbox_requests
 WHERE request_id=$1 AND stored_prefix IS NOT NULL AND stored_deleted_at IS NULL AND status IN('declined','expired','revoked','completed')`, id).Scan(&raw, &prefix, &team, &until)
		if noRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, &m)
	})
	if err != nil || team == "" {
		return err
	}
	var store teambucket.ObjectStore
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		store, err = s.storedBucket(ctx, tx, team)
		return err
	})
	if err != nil {
		return err
	}
	for _, p := range m.Parts {
		key, err := wire.PartKey(prefix, id, p.Index)
		if err == nil {
			err = store.Delete(ctx, key)
		}
		if err != nil {
			return fmt.Errorf("delete stored file part %d: %w", p.Index, err)
		}
	}
	if until != nil && until.After(s.now()) {
		return nil
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE team_inbox_requests SET stored_deleted_at=$2 WHERE request_id=$1 AND (stored_upload_until IS NULL OR stored_upload_until<=$2)`, id, s.now())
		return err
	})
}

func (s *Service) PruneStored(ctx context.Context) error {
	var ids []string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE team_inbox_requests r SET status=CASE WHEN expires_at<=$1 THEN 'expired' ELSE 'revoked' END,updated_at=$1
 WHERE r.request_id IN (SELECT r.request_id FROM team_inbox_requests r WHERE r.stored_prefix IS NOT NULL
 AND r.status IN('pending','approved','consumed') AND (r.expires_at<=$1 OR NOT (`+storedAuthorized+`)) LIMIT 32)`, s.now()); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT request_id FROM team_inbox_requests WHERE stored_prefix IS NOT NULL AND stored_deleted_at IS NULL
 AND status IN('declined','expired','revoked','completed') ORDER BY updated_at LIMIT 8`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		failures = append(failures, s.DeleteStored(ctx, id))
	}
	return errors.Join(failures...)
}
