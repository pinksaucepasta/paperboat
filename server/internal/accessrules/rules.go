// Package accessrules keeps an owner's IP allow and deny lists for one tunnel or preview (edge abuse
// plan 3.5), the only visitor control an owner sets. Edges get them in the ingress snapshot and refuse
// a client address that a deny entry matches, or that no allow entry matches when allow is not empty,
// before any request reaches the origin. Prior art: ngrok's restrict-ips action (allow/deny CIDRs).
package accessrules

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var (
	ErrInvalid  = errors.New("ip rules invalid")
	ErrNotFound = errors.New("resource not found")
)

// MaxEntries bounds each list.
const MaxEntries = 256

// Rules are CIDRs (a bare address means that one address).
type Rules struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Normalize parses every entry, writes it as its masked prefix, drops duplicates and sorts each list.
func Normalize(r Rules) (Rules, error) {
	allow, err := normalizeList("allow", r.Allow)
	if err != nil {
		return Rules{}, err
	}
	deny, err := normalizeList("deny", r.Deny)
	if err != nil {
		return Rules{}, err
	}
	return Rules{Allow: allow, Deny: deny}, nil
}

func normalizeList(name string, entries []string) ([]string, error) {
	if len(entries) > MaxEntries {
		return nil, fmt.Errorf("%w: %s has more than %d entries", ErrInvalid, name, MaxEntries)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		prefix, err := ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("%w: %s entry %q is not an IP address or CIDR", ErrInvalid, name, entry)
		}
		// An allow list of /0 admits one address family only, so it never means "allow everyone"; leave the list empty for that.
		if name == "allow" && prefix.Bits() == 0 {
			return nil, fmt.Errorf("%w: allow entry %q matches every address of its family; remove the allow list to allow everyone", ErrInvalid, entry)
		}
		if s := prefix.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out, nil
}

// ParsePrefix reads a CIDR or a bare address as its masked prefix; IPv4-mapped IPv6 becomes IPv4.
func ParsePrefix(entry string) (netip.Prefix, error) {
	entry = strings.TrimSpace(entry)
	if !strings.Contains(entry, "/") {
		addr, err := netip.ParseAddr(entry)
		if err != nil || addr.Zone() != "" {
			return netip.Prefix{}, ErrInvalid
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}

// Store reads and writes the rules; callers authorize the actor's scope first (API token scopes).
type Store struct{ db *db.DB }

func New(store *db.DB) *Store { return &Store{db: store} }

// manageable is the SQL that decides whether actor may manage the resource, the same rule the tunnel
// and preview management paths use (owner, team managers, machine capability).
func manageable(kind string) (string, error) {
	switch kind {
	case "tunnel":
		return `SELECT paperboat.tunnel_management_allowed($1,$2)`, nil
	case "preview":
		return `SELECT paperboat.preview_management_allowed($1,$2)`, nil
	}
	return "", ErrInvalid
}

func (s *Store) authorizeTx(ctx context.Context, tx *db.Tx, actor, kind, id string) error {
	query, err := manageable(kind)
	if err != nil {
		return err
	}
	var allowed bool
	if err := tx.QueryRow(ctx, query, actor, id).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}

// Get returns the resource's rules (empty lists when none are set).
func (s *Store) Get(ctx context.Context, actor, kind, id string) (Rules, error) {
	rules := Rules{Allow: []string{}, Deny: []string{}}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeTx(ctx, tx, actor, kind, id); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT allow_cidrs,deny_cidrs FROM access_ip_rules WHERE resource_kind=$1 AND resource_id=$2`, kind, id).Scan(&rules.Allow, &rules.Deny)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return rules, err
}

// Put replaces the resource's rules; empty lists remove them.
func (s *Store) Put(ctx context.Context, actor, kind, id string, rules Rules) (Rules, error) {
	rules, err := Normalize(rules)
	if err != nil {
		return Rules{}, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeTx(ctx, tx, actor, kind, id); err != nil {
			return err
		}
		if len(rules.Allow) == 0 && len(rules.Deny) == 0 {
			_, err := tx.Exec(ctx, `DELETE FROM access_ip_rules WHERE resource_kind=$1 AND resource_id=$2`, kind, id)
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO access_ip_rules(resource_kind,resource_id,allow_cidrs,deny_cidrs,updated_by) VALUES($1,$2,$3,$4,$5)
ON CONFLICT (resource_kind,resource_id) DO UPDATE SET allow_cidrs=excluded.allow_cidrs,deny_cidrs=excluded.deny_cidrs,updated_by=excluded.updated_by,updated_at=now()`, kind, id, rules.Allow, rules.Deny, actor)
		return err
	})
	if err != nil {
		return Rules{}, err
	}
	return rules, nil
}
