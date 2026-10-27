package config

import (
	"encoding/json"
	"strings"
)

func (c Config) RedactedJSON() string {
	redacted := c
	if redacted.Secrets.ResendAPIKey != "" {
		redacted.Secrets.ResendAPIKey = "[redacted]"
	}
	if redacted.Secrets.SentryAPIToken != "" {
		redacted.Secrets.SentryAPIToken = "[redacted]"
	}
	if redacted.Secrets.TestFixturesToken != "" {
		redacted.Secrets.TestFixturesToken = "[redacted]"
	}
	if redacted.Secrets.BackupReportToken != "" {
		redacted.Secrets.BackupReportToken = "[redacted]"
	}
	if redacted.Database.DSN != "" {
		redacted.Database.DSN = "[redacted]"
	}
	if redacted.Database.DirectDSN != "" {
		redacted.Database.DirectDSN = "[redacted]"
	}
	if redacted.Secrets.SentryDSN != "" {
		redacted.Secrets.SentryDSN = "[redacted]"
	}
	redacted.Secrets.SessionKeys = redactSlice(redacted.Secrets.SessionKeys)
	redacted.Secrets.MintSigningKeys = redactSlice(redacted.Secrets.MintSigningKeys)
	redacted.Secrets.EncryptionKey = redact(redacted.Secrets.EncryptionKey)
	redacted.Secrets.WorkOSAPIKey = redact(redacted.Secrets.WorkOSAPIKey)
	redacted.Secrets.WorkOSClientID = redact(redacted.Secrets.WorkOSClientID)
	redacted.Secrets.WorkOSActionsSecret = redact(redacted.Secrets.WorkOSActionsSecret)
	redacted.Secrets.StripeAPIKey = redact(redacted.Secrets.StripeAPIKey)
	redacted.Secrets.StripeWebhookSecret = redact(redacted.Secrets.StripeWebhookSecret)
	redacted.Secrets.GitHubClientID = redact(redacted.Secrets.GitHubClientID)
	redacted.Secrets.GitHubClientSecret = redact(redacted.Secrets.GitHubClientSecret)
	redacted.Secrets.EdgeControlCredential = redact(redacted.Secrets.EdgeControlCredential)
	redacted.Secrets.ReceiptEmailToken = redact(redacted.Secrets.ReceiptEmailToken)
	if redacted.Secrets.DiagnosticsAccessKey != "" {
		redacted.Secrets.DiagnosticsAccessKey = "[redacted]"
	}
	if redacted.Secrets.DiagnosticsSecretKey != "" {
		redacted.Secrets.DiagnosticsSecretKey = "[redacted]"
	}
	b, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func redactSlice(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = redact(value)
	}
	return out
}

func redact(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "[redacted]"
	}
	return value[:4] + strings.Repeat("*", 8) + value[len(value)-4:]
}
