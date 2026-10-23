package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
)

// relayFirstGeneration is the lowest grant generation: grants are minted at a configuration generation of at least 1,
// so a revocation floor of 1 or less cuts nothing and is never sent.
const relayFirstGeneration = 1

// relayQueryer is the query surface of the revocation passes; a test counts their statements through it.
type relayQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// relaySubjectSet is a relay's full subject list in the form the revocation queries read it.
type relaySubjectSet struct {
	digest string
	list   []relaySubject
	json   string // the list as a JSON array, the revocation queries' input
}

// newRelaySubjectSet canonicalizes a full list and digests it as relaySubjectsDigest defines.
func newRelaySubjectSet(subjects []relaySubject) (relaySubjectSet, error) {
	if subjects == nil {
		subjects = []relaySubject{}
	}
	data, err := json.Marshal(subjects)
	if err != nil {
		return relaySubjectSet{}, err
	}
	return relaySubjectSet{digest: relaySubjectsDigest(subjects), list: subjects, json: string(data)}, nil
}

// relaySubjectsDigest is the digest a relay sends as subjects_digest: lowercase hex SHA-256 of the lines
// "account_id\x00endpoint_id\n" for the subjects, sorted as strings. Membership only: grant generations change on
// every peer-network refresh, so they are not part of the digest. Identifiers never contain NUL or newline
// (checked on observation), so the lines are unambiguous.
func relaySubjectsDigest(subjects []relaySubject) string {
	lines := make([]string, len(subjects))
	for i, subject := range subjects {
		lines[i] = subject.AccountID + "\x00" + subject.EndpointID + "\n"
	}
	slices.Sort(lines)
	h := sha256.New()
	for _, line := range lines {
		_, _ = io.WriteString(h, line)
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
// stored under it when it differs from the stored one. A full list without a digest (an older relay) is only used.
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

// relayRevocationsByGeneration serves a relay that sent generations with its list (no subjects_digest): one query
// for the whole list, revoking a subject at its identity's relay revocation generation when that is above the
// subject's generation, or at generation+1 when no identity is eligible for this node's pool.
func relayRevocationsByGeneration(ctx context.Context, q relayQueryer, node string, set relaySubjectSet) ([]relayRevocation, error) {
	revoked := []relayRevocation{}
	rows, err := q.QueryContext(ctx, `SELECT s.account_id,s.endpoint_id,coalesce(n.relay_revocation_generation,s.generation+1) FROM jsonb_to_recordset($2::jsonb) AS s(account_id text,endpoint_id text,generation bigint) LEFT JOIN paperboat.peer_network_identities n ON n.user_id=s.account_id AND n.endpoint_id=s.endpoint_id AND paperboat.node_pool_eligible($1,s.account_id,'relay') WHERE coalesce(n.relay_revocation_generation,s.generation+1)>s.generation ORDER BY s.account_id,s.endpoint_id`, node, set.json)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var revocation relayRevocation
		if err := rows.Scan(&revocation.AccountID, &revocation.EndpointID, &revocation.Generation); err != nil {
			return nil, err
		}
		revoked = append(revoked, revocation)
	}
	return revoked, rows.Err()
}

// relayMinGenerations serves a digest relay whose members carry no generation: one query over the whole membership
// gives each member the first grant generation still valid for it (min_generation). An eligible identity's relay
// revocation generation is that floor. An identity no longer eligible for this node's pool is cut at its
// configuration generation+1, above every grant minted for it. A member with no identity is cut at the largest
// generation. Only members above relayFirstGeneration are listed; the rest have nothing to revoke.
func relayMinGenerations(ctx context.Context, q relayQueryer, node string, set relaySubjectSet) ([]relayRevocation, error) {
	revoked := []relayRevocation{}
	rows, err := q.QueryContext(ctx, `SELECT q.account_id,q.endpoint_id,q.min_generation FROM (SELECT s.account_id,s.endpoint_id,CASE WHEN n.user_id IS NULL THEN $3::bigint WHEN paperboat.node_pool_eligible($1,s.account_id,'relay') THEN n.relay_revocation_generation ELSE n.config_generation+1 END AS min_generation FROM jsonb_to_recordset($2::jsonb) AS s(account_id text,endpoint_id text) LEFT JOIN paperboat.peer_network_identities n ON n.user_id=s.account_id AND n.endpoint_id=s.endpoint_id) q WHERE q.min_generation>$4 ORDER BY q.account_id,q.endpoint_id`, node, set.json, int64(math.MaxInt64), int64(relayFirstGeneration))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var revocation relayRevocation
		if err := rows.Scan(&revocation.AccountID, &revocation.EndpointID, &revocation.MinGeneration); err != nil {
			return nil, err
		}
		revoked = append(revoked, revocation)
	}
	return revoked, rows.Err()
}
