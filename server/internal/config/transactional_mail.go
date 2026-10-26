package config

import (
	"errors"
	"net/mail"
	"net/url"
	"time"
)

type TransactionalMail struct {
	Endpoint                  string `json:"endpoint"`
	From                      string `json:"from"`
	ConsoleOrigin             string `json:"console_origin"`
	InvitationLifetimeSeconds int64  `json:"invitation_lifetime_seconds"`
	// InvitationMailDedupSeconds: a new invitation to an address that already got one for the same
	// team within this window sends no mail (pending or cancelled and re-created). Resend always mails.
	InvitationMailDedupSeconds int64 `json:"invitation_mail_dedup_seconds"`
}

func (m TransactionalMail) Validate(key string) error {
	if m.InvitationLifetimeSeconds < 60 || m.InvitationLifetimeSeconds > int64(90*24*time.Hour/time.Second) {
		return errors.New("mail invitation_lifetime_seconds must be between 60 and 7776000")
	}
	if m.InvitationMailDedupSeconds < 0 || m.InvitationMailDedupSeconds > int64(30*24*time.Hour/time.Second) {
		return errors.New("mail invitation_mail_dedup_seconds must be between 0 and 2592000")
	}
	if key == "" {
		return nil
	}
	if _, err := mail.ParseAddress(m.From); err != nil {
		return errors.New("mail.from must be a verified sender address")
	}
	for _, raw := range []string{m.Endpoint, m.ConsoleOrigin} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("mail endpoint and console_origin must be HTTPS URLs without credentials, queries or fragments")
		}
	}
	u, _ := url.Parse(m.ConsoleOrigin)
	if u.Path != "" && u.Path != "/" {
		return errors.New("mail.console_origin must contain only the console origin")
	}
	return nil
}
