package config

import (
	"strings"
	"testing"
)

func TestTransactionalMailConfiguration(t *testing.T) {
	env := map[string]string{"PAPERBOAT_RESEND_API_KEY_FILE": "private-key-file", "PAPERBOAT_MAIL_ENDPOINT": "https://mail.example.test/emails", "PAPERBOAT_MAIL_FROM": "Paperboat <sender@example.test>", "PAPERBOAT_MAIL_CONSOLE_ORIGIN": "https://console.example.test", "PAPERBOAT_INVITATION_LIFETIME_SECONDS": "2592000", "PAPERBOAT_MAIL_ABUSE_MAILBOX": "abuse@mail.example.test"}
	cfg := Default()
	err := overlayEnv(&cfg, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, func(path string) ([]byte, error) {
		if path != "private-key-file" {
			t.Fatal("unexpected key path")
		}
		return []byte("private-test-key\n"), nil
	})
	if err != nil || cfg.Mail.Validate(cfg.Secrets.ResendAPIKey) != nil || cfg.Secrets.ResendAPIKey != "private-test-key" || strings.Contains(cfg.RedactedJSON(), "private-test-key") {
		t.Fatal("mail configuration or redaction failed")
	}
	if cfg.Mail.AbuseMailbox != "abuse@mail.example.test" {
		t.Fatalf("abuse mailbox = %q", cfg.Mail.AbuseMailbox)
	}
	for _, bad := range []string{"not an address", "Abuse <abuse@mail.example.test>", "a@b.test, c@d.test"} {
		withBad := cfg.Mail
		withBad.AbuseMailbox = bad
		if withBad.Validate(cfg.Secrets.ResendAPIKey) == nil {
			t.Fatalf("abuse mailbox %q accepted", bad)
		}
	}
	cfg.Mail.ConsoleOrigin += "?token=private"
	if cfg.Mail.Validate(cfg.Secrets.ResendAPIKey) == nil {
		t.Fatal("token-bearing link accepted")
	}
}
