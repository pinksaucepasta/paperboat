package operatorinfra

import (
	"context"
	"regexp"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var hostnameAfter = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// PublicationBlock is a hostname an operator blocked (or blocked and later unblocked: the row stays as history).
// Version is the revision the next block or unblock must send. The owner account is not shown.
type PublicationBlock struct {
	Hostname  string    `json:"hostname"`
	Blocked   bool      `json:"blocked"`
	Legal     bool      `json:"legal"`
	Reason    string    `json:"reason"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PublicationBlockPage struct {
	Total     int64              `json:"total"`
	Items     []PublicationBlock `json:"items"`
	NextAfter string             `json:"next_after,omitempty"`
}

// ListPublicationBlocks pages publication blocks by hostname; blocked=true narrows to hostnames blocked now.
func (s *Service) ListPublicationBlocks(ctx context.Context, actor string, onlyBlocked bool, after string, limit int) (PublicationBlockPage, error) {
	out := PublicationBlockPage{Items: []PublicationBlock{}}
	if limit < 1 || limit > 100 || (after != "" && !hostnameAfter.MatchString(after)) {
		return PublicationBlockPage{}, ErrInvalid
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := allowRead(ctx, tx, actor); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM publication_blocks WHERE (NOT $1 OR blocked)`, onlyBlocked).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT hostname,blocked,legal,reason,version,created_at,updated_at FROM publication_blocks WHERE (NOT $1 OR blocked) AND hostname>$2 ORDER BY hostname LIMIT $3`, onlyBlocked, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v PublicationBlock
			if err := rows.Scan(&v.Hostname, &v.Blocked, &v.Legal, &v.Reason, &v.Version, &v.CreatedAt, &v.UpdatedAt); err != nil {
				return err
			}
			if len(out.Items) == limit {
				out.NextAfter = out.Items[len(out.Items)-1].Hostname
				break
			}
			out.Items = append(out.Items, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator.infrastructure.publication_blocks_listed", ResourceType: "publication_block", ResourceID: "*"})
	})
	if err != nil {
		return PublicationBlockPage{}, err
	}
	return out, nil
}
