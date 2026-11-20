package announcements

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// NoticeOperators puts one notice in the admin bell. A repeat of the key is a no-op, so a condition that is seen on every
// pass notifies once; a new key (a new expiry date, say) notifies again.
func (s *Service) NoticeOperators(ctx context.Context, key, title, body string, lifetime time.Duration) error {
	_, err := s.store.Pool().Exec(ctx, `INSERT INTO paperboat.announcements(id,kind,title,body,severity,dismissible,audience_kind,audience_value,starts_at,ends_at,created_by,idempotency_key)
VALUES($1,'notice',$2,$3,'warning',true,'operators','',now(),now()+$4::interval,$5,$6) ON CONFLICT (created_by,idempotency_key) DO NOTHING`,
		"ann_"+uuid.NewString(), title, body, lifetime.String(), SystemCreator, key)
	return err
}
