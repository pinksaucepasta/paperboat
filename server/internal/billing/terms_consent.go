package billing

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/legalconsent"
)

// ErrTermsConsentRequired refuses a purchase whose request does not carry the current consent version.
var ErrTermsConsentRequired = errors.New("the purchase needs the current no-refund consent")

// CheckTermsVersion refuses a purchase request that does not carry the current consent version.
func CheckTermsVersion(version string) error {
	if version != legalconsent.Version {
		return ErrTermsConsentRequired
	}
	return nil
}

// RecordTerms stores who accepted which wording and when. The handler calls it before it returns a checkout
// URL, so every URL a buyer can pay at has a record; a retry of the same operation keeps the first one.
func (s *WorkspaceService) RecordTerms(ctx context.Context, operationID, actor, kind string) error {
	_, err := s.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_terms_consents(operation_id,actor_user_id,kind,terms_version,text_sha256) VALUES($1,$2,$3,$4,$5) ON CONFLICT (operation_id) DO NOTHING`,
		operationID, actor, kind, legalconsent.Version, legalconsent.TextSHA256())
	return err
}
