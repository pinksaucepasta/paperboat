package operatoractions

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/ipblocks"
)

func isIPBlockKind(kind string) bool { return kind == "ip_block_add" || kind == "ip_block_remove" }

// MaxBanIPBlocks bounds the networks one ban can block along with the account.
const MaxBanIPBlocks = 20

// ipBlockAction blocks a client network from sign-up, sign-in and CLI login (ip_block_add: the range is in Input.CIDR, the
// optional end in Input.EndsAt), or lifts a block (ip_block_remove: the target is the block's id). Sessions that exist stay.
func ipBlockAction(ctx context.Context, tx *db.Tx, actor, kind, target, reason string, in Input, out *Result) error {
	if kind == "ip_block_remove" {
		b, err := ipblocks.RemoveTx(ctx, tx, actor, target)
		if err != nil {
			return mapIPBlockError(err)
		}
		*out = Result{ResourceID: b.ID, State: "lifted"}
		return nil
	}
	prefix, err := ipblocks.Parse(in.CIDR)
	if err != nil {
		return ErrInvalid
	}
	var endsAt *time.Time
	if in.EndsAt != "" {
		t, err := time.Parse(time.RFC3339, in.EndsAt)
		if err != nil {
			return ErrInvalid
		}
		t = t.UTC()
		endsAt = &t
	}
	b, err := ipblocks.AddTx(ctx, tx, actor, prefix, reason, endsAt)
	if err != nil {
		return mapIPBlockError(err)
	}
	*out = Result{ResourceID: b.ID, State: "blocked", ExpiresAt: endsAt}
	return nil
}

func mapIPBlockError(err error) error {
	switch {
	case errors.Is(err, ipblocks.ErrInvalid):
		return ErrInvalid
	case errors.Is(err, ipblocks.ErrExists):
		return ErrConflict
	case errors.Is(err, ipblocks.ErrNotFound):
		return ErrNotFound
	}
	return err
}

// blockBanNetworks blocks the networks an operator chose to block along with a ban; one already blocked stays as it is.
func blockBanNetworks(ctx context.Context, tx *db.Tx, actor, reason string, entries []string) error {
	for _, entry := range entries {
		prefix, err := ipblocks.Parse(entry)
		if err != nil {
			return ErrInvalid
		}
		if _, err = ipblocks.AddTx(ctx, tx, actor, prefix, reason, nil); err != nil && !errors.Is(err, ipblocks.ErrExists) {
			return mapIPBlockError(err)
		}
	}
	return nil
}
