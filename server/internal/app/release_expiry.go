package app

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/releases"
)

// releaseExpiryNotices tells operators in the admin bell, once per role and expiry, that signed release metadata is
// close to expiring or has expired. Past that point every client's update check fails closed. The fix is the runbook
// server/docs/runbooks/release-metadata.md.
func releaseExpiryNotices(ctx context.Context, directory string, notices *announcements.Service, now time.Time) error {
	expiries, err := releases.ReadMetadataExpiry(directory)
	if err != nil {
		return err
	}
	for _, w := range releases.ExpiryWarnings(expiries, now) {
		when, title := "expires "+w.Expires.UTC().Format(time.RFC3339), w.Role+" release metadata expires soon"
		if w.Expired {
			when, title = "expired "+w.Expires.UTC().Format(time.RFC3339), w.Role+" release metadata has expired"
		}
		key := "release-metadata:" + w.Role + ":" + w.Expires.UTC().Format(time.RFC3339)
		body := "The signed " + w.Role + " metadata " + when + ". Clients stop updating once it lapses. Re-sign it as the release-metadata runbook says."
		if err := notices.NoticeOperators(ctx, key, title, body, 30*24*time.Hour); err != nil {
			return err
		}
	}
	return nil
}
