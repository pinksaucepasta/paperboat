// Package operatoractions executes the operator's mutating actions: banning or unbanning accounts and
// teams, draining or resuming relays, disabling or enabling tunnels, and blocking or unblocking a
// publication. Execute requires an idempotency key and a reason, writes the audit record in the same
// transaction as the change, and can notify the owner by mail when a takedown applies.
package operatoractions

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// publicationAction takes one hostname down for violating the acceptable use policy, or restores it
// (edge abuse plan 3.6.3). The server then withholds the name's ingress decisions and lists it in
// every edge snapshot, so the edge answers the takedown page without contacting the origin. The row
// stays after an unblock (evidence and history); revision is 1 for a name never blocked, else the
// row's version. A block mails the owning account once per version.
func (s *Service) publicationAction(ctx context.Context, tx *db.Tx, actorID, target, reason string, block bool, input Input, out *Result) error {
	host := strings.ToLower(strings.TrimSuffix(target, "."))
	if !publicationHostPattern.MatchString(host) {
		return ErrInvalid
	}
	var blocked, legal bool
	var version int64
	err := tx.QueryRow(ctx, `SELECT blocked,legal,version FROM publication_blocks WHERE hostname=$1 FOR UPDATE`, host).Scan(&blocked, &legal, &version)
	switch {
	case operatorNoRowsOnly(err):
		if !block {
			return ErrNotFound
		}
		if input.Revision != 1 {
			return ErrConflict
		}
		version = 1
		if _, err = tx.Exec(ctx, `INSERT INTO publication_blocks(hostname,blocked,legal,reason,account_id) VALUES($1,true,$2,$3,$4)`, host, input.Legal, reason, ownerAccountTx(ctx, tx, host)); err != nil {
			return err
		}
	case err != nil:
		return err
	case input.Revision != version:
		return ErrConflict
	case blocked == block && (!block || legal == input.Legal):
		// Already in the wanted state: the result repeats it.
	default:
		version++
		if _, err = tx.Exec(ctx, `UPDATE publication_blocks SET blocked=$2,legal=$3,reason=$4,version=$5,updated_at=now() WHERE hostname=$1`, host, block, block && input.Legal, reason, version); err != nil {
			return err
		}
	}
	state := "active"
	if block {
		state = "blocked"
		// A block whose reference is an open abuse report about this hostname handles that report.
		marked, err := tx.Exec(ctx, `UPDATE abuse_reports SET status='actioned', triaged_by=$3, triaged_at=now(), triage_note='Hostname blocked.'
 WHERE id=$1 AND hostname=$2 AND status='open'`, reason, host, actorID)
		if err != nil {
			return err
		}
		if marked.RowsAffected() == 1 {
			if err = s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "abuse_report.triaged", ResourceType: "abuse_report", ResourceID: reason, Metadata: map[string]any{"status": "actioned", "note": "Hostname blocked.", "reference": reason, "hostname": host}}); err != nil {
				return err
			}
		}
		if err = s.noticeBlockedTx(ctx, tx, host, reason, version); err != nil {
			return err
		}
	}
	*out = Result{ResourceID: host, State: state, Version: version}
	return nil
}

// publicationHostPattern is a DNS name: the host reaches a mail Subject and the edge snapshot, so no other character gets in.
var publicationHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// ownerAccountTx is the account serving hostname, or nil when no live preview or tunnel route holds it.
func ownerAccountTx(ctx context.Context, tx *db.Tx, host string) any {
	var account string
	if err := tx.QueryRow(ctx, `SELECT e.account_id FROM endpoint_routes r JOIN endpoints e ON e.id=r.endpoint_id
WHERE r.hostname=$1 ORDER BY (e.desired_state='active') DESC, e.updated_at DESC LIMIT 1`, host).Scan(&account); err != nil {
		return nil
	}
	return account
}

// noticeBlockedTx mails the owning account that its address was taken down, once per block version.
func (s *Service) noticeBlockedTx(ctx context.Context, tx *db.Tx, host, reason string, version int64) error {
	if !s.mailEnabled {
		return nil
	}
	var address string
	err := tx.QueryRow(ctx, `SELECT u.primary_email FROM publication_blocks b JOIN users u ON u.id=b.account_id
WHERE b.hostname=$1 AND u.email_verified AND u.primary_email<>''`, host).Scan(&address)
	if operatorNoRowsOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	message := blockedMessage(s.mail, address, host, reason)
	_, err = transactionalmail.EnqueueOnceTx(ctx, tx, fmt.Sprintf("publication-blocked:%s:%d", host, version), message)
	return err
}

func blockedMessage(policy config.TransactionalMail, address, host, reason string) transactionalmail.Message {
	contact := "reply to this email"
	if policy.AbuseMailbox != "" {
		contact = "write to " + policy.AbuseMailbox
	}
	return transactionalmail.Compose(policy, address, "Paperboat: "+host+" was disabled", transactionalmail.Content{
		Heading: host + " was disabled",
		Paragraphs: []string{
			fmt.Sprintf("Paperboat disabled %s because it violated Paperboat's acceptable use policy. Visitors now see a notice instead of your app, and the address stays disabled.", host),
			fmt.Sprintf("Reference: %s. If you think this is a mistake, %s with that reference.", reason, contact),
		},
		Why: "You got this because the disabled address belongs to your account.",
	})
}
