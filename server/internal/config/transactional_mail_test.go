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

func TestMailRecordOnlyConfiguration(t *testing.T) {
	overlay := func(env map[string]string) (Config, error) {
		cfg := Default()
		err := overlayEnv(&cfg, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, nil)
		return cfg, err
	}
	for _, value := range []string{"", "false"} {
		cfg, err := overlay(map[string]string{"PAPERBOAT_MAIL_RECORD_ONLY": value})
		if err != nil || cfg.Mail.RecordOnly || cfg.Mail.Enabled("") || !cfg.Mail.Enabled("key") || cfg.Mail.Validate("") != nil {
			t.Fatalf("record-only %q: err=%v mail=%+v", value, err, cfg.Mail)
		}
	}
	if _, err := overlay(map[string]string{"PAPERBOAT_MAIL_RECORD_ONLY": "yes please"}); err == nil {
		t.Fatal("non-boolean record-only accepted")
	}
	cfg, err := overlay(map[string]string{"PAPERBOAT_MAIL_RECORD_ONLY": "true"})
	if err != nil || !cfg.Mail.RecordOnly || !cfg.Mail.Enabled("") {
		t.Fatalf("record-only did not enable producers: err=%v", err)
	}
	// Producers compose real messages, so the sender and links are required; the endpoint is never called.
	if cfg.Mail.Validate("") == nil {
		t.Fatal("record-only accepted without a sender and console origin")
	}
	cfg.Mail.From, cfg.Mail.ConsoleOrigin = "Paperboat <sender@example.test>", "https://console.example.test"
	if err := cfg.Mail.Validate(""); err != nil {
		t.Fatal(err)
	}
}
