package operatoractions

import (
	"context"
	"fmt"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// noticeBannedTx tells the person whose account (or the owner whose team) was banned what happened, why and how to
// appeal, as the Acceptable Use Policy promises. It is queued in the ban's transaction under the ban's id, so a replay
// never mails twice.
func (s *Service) noticeBannedTx(ctx context.Context, tx *db.Tx, account, banID, subject, reason string) error {
	if !s.mailEnabled {
		return nil
	}
	var address string
	err := tx.QueryRow(ctx, `SELECT primary_email FROM users WHERE id=$1 AND email_verified AND primary_email<>''`, account).Scan(&address)
	if operatorNoRowsOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	message := bannedMessage(s.mail, address, subject, reason, banID)
	_, err = transactionalmail.EnqueueOnceTx(ctx, tx, "account-banned:"+banID, message)
	return err
}

func bannedMessage(policy config.TransactionalMail, address, subject, reason, banID string) transactionalmail.Message {
	appeal := "reply to this email"
	if policy.AbuseMailbox != "" {
		appeal = "write to " + policy.AbuseMailbox
	}
	return transactionalmail.Compose(policy, address, "Paperboat: "+subject+" was banned", transactionalmail.Content{
		Heading: subject + " was banned",
		Paragraphs: []string{
			fmt.Sprintf("Paperboat banned %s because it broke the Terms of Service or the Acceptable Use Policy. Sign-in sessions and tokens ended, machines can no longer connect, and public links stopped. Your data is kept.", subject),
			fmt.Sprintf("Reason: %s. Reference for an appeal: %s.", reason, banID),
			fmt.Sprintf("If you think we got it wrong, %s and quote that reference. A person reviews every appeal, and we restore what we blocked if it was a mistake.", appeal),
		},
		Why: "You got this because the ban applies to your account.",
	})
}
