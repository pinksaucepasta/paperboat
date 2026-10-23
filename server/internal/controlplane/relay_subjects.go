package controlplane

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// relaySubjectSet is a relay's full subject list in the form the revocation query reads it.
type relaySubjectSet struct {
	digest string
	list   []relaySubject // sorted by (account_id, endpoint_id)
	json   string         // list as a JSON array, the revocation query's input
}

// newRelaySubjectSet canonicalizes a full list: sorted by (account_id, endpoint_id), digested as
// relaySubjectsDigest defines it.
func newRelaySubjectSet(subjects []relaySubject) (relaySubjectSet, error) {
	sorted := make([]relaySubject, len(subjects))
	copy(sorted, subjects)
	slices.SortFunc(sorted, func(a, b relaySubject) int {
		return cmp.Or(strings.Compare(a.AccountID, b.AccountID), strings.Compare(a.EndpointID, b.EndpointID))
	})
	data, err := json.Marshal(sorted)
	if err != nil {
		return relaySubjectSet{}, err
	}
	return relaySubjectSet{digest: relaySubjectsDigest(sorted), list: sorted, json: string(data)}, nil
}

// relaySubjectsDigest is the digest a relay sends as subjects_digest: lowercase hex SHA-256 over the
// subjects sorted by (account_id, endpoint_id), each as one "account_id\x00endpoint_id\x00generation\n" line.
// Identifiers never contain NUL or newline (checked on observation), so the lines are unambiguous.
func relaySubjectsDigest(sorted []relaySubject) string {
	h := sha256.New()
	var line []byte
	for _, subject := range sorted {
		line = append(line[:0], subject.AccountID...)
		line = append(line, 0)
		line = append(line, subject.EndpointID...)
		line = append(line, 0)
		line = strconv.AppendInt(line, subject.Generation, 10)
		line = append(line, '\n')
		h.Write(line)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validRelaySubjectsDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// relayObservedSubjects is the subject list an observation is judged against. A digest-only observation uses
// the list stored for the node's current generation and epoch when its digest matches; otherwise it asks for the
// full list (required) and changes nothing. A full list is checked against the digest when one is sent, and
// stored under it. A full list without a digest (an older relay) is only used.
func relayObservedSubjects(ctx context.Context, tx *sql.Tx, in relayNodeObservation) (set relaySubjectSet, required bool, err error) {
	if in.Subjects == nil && in.SubjectsDigest != "" {
		stored, found, err := storedRelaySubjects(ctx, tx, in.NodeID, in.ProcessEpoch, in.Generation)
		if err != nil {
			return relaySubjectSet{}, false, err
		}
		if !found || stored.digest != in.SubjectsDigest {
			return relaySubjectSet{}, true, nil
		}
		return stored, false, nil
	}
	set, err = newRelaySubjectSet(in.Subjects)
	if err != nil {
		return relaySubjectSet{}, false, err
	}
	if in.SubjectsDigest != "" {
		if set.digest != in.SubjectsDigest {
			return relaySubjectSet{}, false, invalidUsage("relay_subjects.go:digest")
		}
		if err := storeRelaySubjects(ctx, tx, in.NodeID, in.ProcessEpoch, in.Generation, set); err != nil {
			return relaySubjectSet{}, false, err
		}
	}
	return set, false, nil
}

// storedRelaySubjects loads the list stored for a node's generation and epoch, if any.
func storedRelaySubjects(ctx context.Context, tx *sql.Tx, node, epoch string, generation int64) (relaySubjectSet, bool, error) {
	var set relaySubjectSet
	err := tx.QueryRowContext(ctx, `SELECT digest,subjects::text FROM paperboat.relay_subject_sets WHERE node_id=$1 AND process_epoch=$2 AND node_generation=$3`, node, epoch, generation).Scan(&set.digest, &set.json)
	if errors.Is(err, sql.ErrNoRows) {
		return relaySubjectSet{}, false, nil
	}
	if err != nil {
		return relaySubjectSet{}, false, err
	}
	if err := json.Unmarshal([]byte(set.json), &set.list); err != nil {
		return relaySubjectSet{}, false, err
	}
	return set, true, nil
}

// storeRelaySubjects keeps the set as the node's list for its current generation and epoch and drops the lists of
// its other generations and epochs, so the table holds at most one list per node. An unchanged list is not rewritten.
func storeRelaySubjects(ctx context.Context, tx *sql.Tx, node, epoch string, generation int64, set relaySubjectSet) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM paperboat.relay_subject_sets WHERE node_id=$1 AND (process_epoch<>$2 OR node_generation<>$3)`, node, epoch, generation); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO paperboat.relay_subject_sets(node_id,process_epoch,node_generation,digest,subjects) VALUES($1,$2,$3,$4,$5::jsonb) ON CONFLICT (node_id,process_epoch,node_generation) DO UPDATE SET digest=EXCLUDED.digest,subjects=EXCLUDED.subjects,stored_at=now() WHERE paperboat.relay_subject_sets.digest<>EXCLUDED.digest`, node, epoch, generation, set.digest, set.json)
	return err
}

// relayRevocations answers the whole set in one query: a subject is revoked at its identity's relay revocation
// generation when that is above the subject's generation, or at its generation+1 when no identity is eligible
// for this node's pool.
func relayRevocations(ctx context.Context, tx *sql.Tx, node string, set relaySubjectSet) ([]relaySubject, error) {
	revoked := []relaySubject{}
	rows, err := tx.QueryContext(ctx, `SELECT s.account_id,s.endpoint_id,coalesce(n.relay_revocation_generation,s.generation+1) FROM jsonb_to_recordset($2::jsonb) AS s(account_id text,endpoint_id text,generation bigint) LEFT JOIN paperboat.peer_network_identities n ON n.user_id=s.account_id AND n.endpoint_id=s.endpoint_id AND paperboat.node_pool_eligible($1,s.account_id,'relay') WHERE coalesce(n.relay_revocation_generation,s.generation+1)>s.generation ORDER BY s.account_id,s.endpoint_id`, node, set.json)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var subject relaySubject
		if err := rows.Scan(&subject.AccountID, &subject.EndpointID, &subject.Generation); err != nil {
			return nil, err
		}
		revoked = append(revoked, subject)
	}
	return revoked, rows.Err()
}
