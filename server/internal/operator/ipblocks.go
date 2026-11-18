package operator

import (
	"context"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/ipblocks"
)

// RecentIP is an address an account signed in or logged in from, with when it was last seen and how often.
type RecentIP struct {
	IP       string    `json:"ip"`
	LastSeen time.Time `json:"last_seen"`
	Count    int64     `json:"count"`
	// Blocked: a running block contains the address.
	Blocked bool `json:"blocked"`
}

// AccountIPs are the addresses of an account's recent sign-ins, sign-ups and CLI logins, read from the audit log (whose
// retention bounds how far back they go). Only operators see them.
type AccountIPs struct {
	AccountID string     `json:"account_id"`
	IPs       []RecentIP `json:"ips"`
}

// maxRecentIPs bounds the addresses one account shows.
const maxRecentIPs = 25

var loginEvents = []string{"auth.login", "auth.device.completed", "auth.client.approved", "auth.client.api_token", "auth.client.recipe"}

func readAccountIPs(ctx context.Context, tx *db.Tx, account string) (AccountIPs, error) {
	out := AccountIPs{AccountID: account, IPs: []RecentIP{}}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, account).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT client_ip,max(created_at),count(*) FROM audit_events
 WHERE actor_user_id=$1 AND event_type=ANY($2::text[]) AND client_ip IS NOT NULL
 GROUP BY client_ip ORDER BY max(created_at) DESC LIMIT $3`, account, loginEvents, maxRecentIPs)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v RecentIP
		if err = rows.Scan(&v.IP, &v.LastSeen, &v.Count); err != nil {
			rows.Close()
			return out, err
		}
		out.IPs = append(out.IPs, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	for i := range out.IPs {
		err = ipblocks.Check(ctx, tx, out.IPs[i].IP)
		out.IPs[i].Blocked = err == ipblocks.ErrBlocked
		if err != nil && err != ipblocks.ErrBlocked {
			return out, err
		}
	}
	return out, nil
}

// IPBlockPage lists operator IP blocks, newest first.
type IPBlockPage struct {
	Counted
	Blocks    []ipblocks.Block `json:"blocks"`
	NextAfter string           `json:"next_after,omitempty"`
}

// readIPBlocks lists blocks, filter state: running (not lifted, not expired) or ended; empty lists all.
func readIPBlocks(ctx context.Context, tx *db.Tx, state, after string, limit int) (_ IPBlockPage, resultErr error) {
	if state != "" && state != "running" && state != "ended" {
		return IPBlockPage{}, ErrInvalid
	}
	cursor := int64(0)
	if after != "" {
		n, err := strconv.ParseInt(after, 10, 64)
		if err != nil || n < 1 {
			return IPBlockPage{}, ErrInvalid
		}
		cursor = n
	}
	const running = `(revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now()))`
	where := ` ip_blocks WHERE ($1='' OR ($1='running')=` + running + `)`
	p := IPBlockPage{Blocks: make([]ipblocks.Block, 0, limit)}
	var err error
	if p.Counted, err = countRows(ctx, tx, where, state); err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `SELECT id,cidr::text,reason,created_by,created_at,expires_at,revoked_at,`+running+`,seq FROM`+where+` AND ($2=0 OR seq<$2) ORDER BY seq DESC LIMIT $3`, state, cursor, limit+1)
	if err != nil {
		return p, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	var lastSeq int64
	for rows.Next() {
		var b ipblocks.Block
		var seq int64
		if err := rows.Scan(&b.ID, &b.CIDR, &b.Reason, &b.CreatedBy, &b.CreatedAt, &b.ExpiresAt, &b.RevokedAt, &b.Running, &seq); err != nil {
			return p, err
		}
		if len(p.Blocks) == limit {
			p.NextAfter = strconv.FormatInt(lastSeq, 10)
			break
		}
		p.Blocks = append(p.Blocks, b)
		lastSeq = seq
	}
	return p, rows.Err()
}
