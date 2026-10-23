package peersessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"sort"
	"time"
)

// The personal mesh of an account (every endpoint of it is a peer of every other, PLAN 3b "Peer network deltas")
// is built once per change into the shared, ordered log peer_mesh_log (migration 150), so an endpoint's refresh
// reads the entries after the mesh_seq it holds instead of loading every peer (3 queries each) again.

// meshMaxAge bounds how stale the shared mesh may be when no topic signalled a change (a CLI session ended without
// its identity, a user deactivated): a refresh after it rebuilds, about as fresh as the configuration lifetime.
const meshMaxAge = time.Minute

type meshChange struct {
	EndpointID string
	Op         string          // add | update | remove
	Entry      *networkBinding // nil for remove
}

// meshEntriesQuery is loadNetworkBinding (admitted = false) and personalNetworkPeers' membership for every endpoint
// of one account at once: personal machines and active CLI sessions with a current certificate.
const meshEntriesQuery = `SELECT n.endpoint_id,n.role,coalesce(n.machine_id,''),n.endpoint_generation,n.machine_generation,n.key_generation,
 n.wireguard_public_key,n.disco_public_key,n.quic_certificate_fingerprint,c.quic_public_key,host(n.virtual_address),c.expires_at
FROM paperboat.peer_network_identities n
JOIN paperboat.users u ON u.id=n.user_id AND u.status='active'
JOIN paperboat.peer_endpoint_certificates c ON c.fingerprint=n.quic_certificate_fingerprint AND c.user_id=n.user_id AND c.endpoint_id=n.endpoint_id
 AND c.generation=n.endpoint_generation AND c.role=n.role AND c.revoked_at IS NULL AND c.issued_at<=$2 AND c.expires_at>$2
 AND NOT EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_revocations r WHERE r.certificate_fingerprint=c.fingerprint)
WHERE n.user_id=$1 AND n.revoked_at IS NULL AND (
 (n.role='machine' AND EXISTS(SELECT 1 FROM paperboat.user_machines m
  JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL
  WHERE m.id=n.endpoint_id AND m.id=n.machine_id AND m.user_id=n.user_id AND m.owner_team_id IS NULL AND m.state IN ('online','offline')
   AND m.seat_state='occupied' AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.installation_generation=n.machine_generation))
 OR (n.role='cli' AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions s WHERE s.id=n.endpoint_id AND s.user_id=n.user_id AND s.state='active' AND s.revoked_at IS NULL)))
ORDER BY n.endpoint_id LIMIT $3`

// buildMesh reads the account's mesh entries and the earliest certificate expiry among them.
func buildMesh(ctx context.Context, tx *sql.Tx, account string, now time.Time) (map[string]networkBinding, time.Time, error) {
	rows, err := tx.QueryContext(ctx, meshEntriesQuery, account, now, maximumAccountEndpoints+1)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	out := map[string]networkBinding{}
	var earliest time.Time
	for rows.Next() {
		b := networkBinding{AccountID: account}
		var key, disco, fingerprint, quicPublic []byte
		var expiry time.Time
		if err := rows.Scan(&b.EndpointID, &b.Role, &b.MachineID, &b.EndpointGeneration, &b.MachineGeneration, &b.KeyGeneration,
			&key, &disco, &fingerprint, &quicPublic, &b.VirtualAddress, &expiry); err != nil {
			return nil, time.Time{}, err
		}
		if len(out) == maximumAccountEndpoints {
			return nil, time.Time{}, ErrResourceLimit
		}
		// The same shape checks as loadNetworkBinding: an entry it would refuse is not a peer.
		if len(quicPublic) != 32 {
			continue
		}
		if _, err := netip.ParseAddr(b.VirtualAddress); err != nil {
			continue
		}
		b.WireGuardPublicKey = base64.RawURLEncoding.EncodeToString(key)
		if len(disco) == 32 {
			b.DiscoPublicKey = base64.RawURLEncoding.EncodeToString(disco)
		}
		b.QUICCertificateFingerprint = hex.EncodeToString(fingerprint)
		b.QUICPublicKey = base64.RawURLEncoding.EncodeToString(quicPublic)
		out[b.EndpointID] = b
		if earliest.IsZero() || expiry.Before(earliest) {
			earliest = expiry
		}
	}
	return out, earliest, rows.Err()
}

// meshSignal is the account's change signal: its peer_network and inventory topic generations (migration 149).
func meshSignal(ctx context.Context, tx *sql.Tx, account string) (int64, error) {
	var signal int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(generation),0)::bigint FROM paperboat.account_topic_generations
 WHERE account_id=$1 AND topic IN ('peer_network','inventory')`, account).Scan(&signal)
	return signal, err
}

// latestMeshEntries is the account's mesh as the log holds it at seq (endpoint -> entry; removed endpoints absent).
func latestMeshEntries(ctx context.Context, tx *sql.Tx, account string, seq int64, endpoints []string) (map[string]networkBinding, error) {
	query := `SELECT DISTINCT ON (endpoint_id) endpoint_id,op,entry FROM paperboat.peer_mesh_log
 WHERE account_id=$1 AND mesh_seq<=$2 AND ($3::text[] IS NULL OR endpoint_id=ANY($3)) ORDER BY endpoint_id,mesh_seq DESC`
	var filter any
	if endpoints != nil {
		filter = endpoints
	}
	rows, err := tx.QueryContext(ctx, query, account, seq, filter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]networkBinding{}
	for rows.Next() {
		var endpoint, op string
		var raw []byte
		if err := rows.Scan(&endpoint, &op, &raw); err != nil {
			return nil, err
		}
		if op == "remove" {
			continue
		}
		var b networkBinding
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		out[endpoint] = b
	}
	return out, rows.Err()
}

// refreshMesh brings the account's shared mesh up to date and returns its mesh_seq. One builder per account at a
// time (a transaction advisory lock); the others then find it fresh. A rebuild reads the account's endpoints once,
// set-based, and appends only the entries that changed: a join or leave writes one row however large the account.
func (s *NetworkService) refreshMesh(ctx context.Context, account string) (int64, error) {
	now := s.now().UTC()
	tx, err := s.store.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('peer_mesh:'||$1,0))`, account); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.peer_mesh_accounts(account_id) VALUES($1) ON CONFLICT DO NOTHING`, account); err != nil {
		return 0, err
	}
	var seq, builtSignal int64
	var builtAt, validUntil sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT mesh_seq,built_signal,built_at,valid_until FROM paperboat.peer_mesh_accounts WHERE account_id=$1`, account).
		Scan(&seq, &builtSignal, &builtAt, &validUntil); err != nil {
		return 0, err
	}
	signal, err := meshSignal(ctx, tx, account)
	if err != nil {
		return 0, err
	}
	if builtAt.Valid && signal == builtSignal && now.Sub(builtAt.Time) < meshMaxAge && (!validUntil.Valid || now.Before(validUntil.Time)) {
		return seq, tx.Commit()
	}
	current, earliest, err := buildMesh(ctx, tx, account, now)
	if err != nil {
		return 0, err
	}
	previous, err := latestMeshEntries(ctx, tx, account, seq, nil)
	if err != nil {
		return 0, err
	}
	for _, change := range diffMesh(previous, current) {
		seq++
		var entry any
		if change.Entry != nil {
			raw, err := json.Marshal(change.Entry)
			if err != nil {
				return 0, err
			}
			entry = raw
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.peer_mesh_log(account_id,mesh_seq,endpoint_id,op,entry) VALUES($1,$2,$3,$4,$5)`,
			account, seq, change.EndpointID, change.Op, entry); err != nil {
			return 0, err
		}
	}
	var until any
	if !earliest.IsZero() {
		until = earliest
	}
	if _, err = tx.ExecContext(ctx, `UPDATE paperboat.peer_mesh_accounts SET mesh_seq=$2,built_signal=$3,built_at=$4,valid_until=$5 WHERE account_id=$1`,
		account, seq, signal, now, until); err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// diffMesh lists the changes from previous to current, ordered by endpoint for a deterministic log.
func diffMesh(previous, current map[string]networkBinding) []meshChange {
	var changes []meshChange
	for _, id := range sortedKeys(previous, current) {
		old, had := previous[id]
		next, has := current[id]
		switch {
		case had && !has:
			changes = append(changes, meshChange{EndpointID: id, Op: "remove"})
		case !had && has:
			entry := next
			changes = append(changes, meshChange{EndpointID: id, Op: "add", Entry: &entry})
		case had && has && !sameBinding(old, next):
			entry := next
			changes = append(changes, meshChange{EndpointID: id, Op: "update", Entry: &entry})
		}
	}
	return changes
}

func sameBinding(a, b networkBinding) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

func sortedKeys(maps ...map[string]networkBinding) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}
