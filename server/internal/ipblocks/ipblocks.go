// Package ipblocks is the operator's deny list of client networks. A running block (not revoked, not past its expiry) refuses
// sign-up, sign-in and CLI login from an address it contains; sessions that already exist are not cut by address alone.
// The address checked is the trusted client address the server derives behind its proxy (never a raw X-Forwarded-For).
package ipblocks

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/accessrules"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var (
	// ErrBlocked means the address is inside a running block.
	ErrBlocked = errors.New("network is blocked")
	// ErrInvalid is an address or range that cannot be blocked, or a bad expiry.
	ErrInvalid = errors.New("invalid ip block")
	// ErrExists means a running block of exactly this range exists already.
	ErrExists = errors.New("ip block exists")
	// ErrNotFound is no running block with the id.
	ErrNotFound = errors.New("ip block not found")
)

// Shortest prefixes that may be blocked: a typo must not shut out a continent.
const (
	MinBitsIPv4 = 8
	MinBitsIPv6 = 16
)

const running = `revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now())`

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Block is one row of ip_blocks.
type Block struct {
	ID        string     `json:"id"`
	CIDR      string     `json:"cidr"`
	Reason    string     `json:"reason"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Running   bool       `json:"running"`
}

// Parse reads an address or CIDR as the masked range to block (IPv4-mapped IPv6 becomes IPv4).
func Parse(entry string) (netip.Prefix, error) {
	prefix, err := accessrules.ParsePrefix(entry)
	if err != nil || !prefix.IsValid() {
		return netip.Prefix{}, fmt.Errorf("%w: %q is not an IP address or CIDR", ErrInvalid, entry)
	}
	if min := MinBitsIPv6; prefix.Addr().Is4() {
		if prefix.Bits() < MinBitsIPv4 {
			return netip.Prefix{}, fmt.Errorf("%w: %s is wider than /%d", ErrInvalid, prefix, MinBitsIPv4)
		}
	} else if prefix.Bits() < min {
		return netip.Prefix{}, fmt.Errorf("%w: %s is wider than /%d", ErrInvalid, prefix, min)
	}
	return prefix, nil
}

// Check returns ErrBlocked when ip is inside a running block. An empty or unreadable address is never blocked: without an
// address there is nothing to compare, and the caller's other limits still apply.
func Check(ctx context.Context, q queryer, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	addr = addr.Unmap().WithZone("")
	var blocked bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ip_blocks WHERE `+running+` AND $1::inet <<= cidr)`, addr.String()).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return ErrBlocked
	}
	return nil
}

// AddTx blocks the range. A block of the same range that has run out is replaced; one still running is ErrExists.
func AddTx(ctx context.Context, tx *db.Tx, actor string, prefix netip.Prefix, reason string, expiresAt *time.Time) (Block, error) {
	if expiresAt != nil {
		// Judged by the database's clock, the one that decides whether a block runs.
		var future bool
		if err := tx.QueryRow(ctx, `SELECT $1::timestamptz>now()`, expiresAt).Scan(&future); err != nil {
			return Block{}, err
		}
		if !future {
			return Block{}, fmt.Errorf("%w: the expiry is in the past", ErrInvalid)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE ip_blocks SET revoked_at=now(),revoked_by='expired' WHERE cidr=$1::cidr AND revoked_at IS NULL AND expires_at<=now()`, prefix.String()); err != nil {
		return Block{}, err
	}
	b := Block{ID: "ipb_" + uuid.NewString(), CIDR: prefix.String(), Reason: reason, CreatedBy: actor, ExpiresAt: expiresAt, Running: true}
	err := tx.QueryRow(ctx, `INSERT INTO ip_blocks(id,cidr,reason,created_by,expires_at) VALUES($1,$2::cidr,$3,$4,$5) ON CONFLICT DO NOTHING RETURNING created_at`,
		b.ID, b.CIDR, reason, actor, expiresAt).Scan(&b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Block{}, ErrExists
	}
	return b, err
}

// RemoveTx ends a running block now.
func RemoveTx(ctx context.Context, tx *db.Tx, actor, id string) (Block, error) {
	var b Block
	var cidr string
	err := tx.QueryRow(ctx, `UPDATE ip_blocks SET revoked_at=now(),revoked_by=$2 WHERE id=$1 AND `+running+` RETURNING id,cidr::text,reason,created_by,created_at,expires_at,revoked_at`, id, actor).
		Scan(&b.ID, &cidr, &b.Reason, &b.CreatedBy, &b.CreatedAt, &b.ExpiresAt, &b.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Block{}, ErrNotFound
	}
	b.CIDR = cidr
	return b, err
}
