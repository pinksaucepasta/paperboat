package supportbundle

import (
	"regexp"

	"github.com/pinksaucepasta/paperboat/shared/redact"
)

// redactionRules run in order: the private-key block and header lines first, the broad URL, path and hostname rules last.
var redactionRules = []redact.Rule{
	{Pattern: regexp.MustCompile(`(?is)-----BEGIN (?:[A-Z0-9 ]* )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]* )?PRIVATE KEY-----`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?im)^(\s*(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token|x-csrf-token)\s*:\s*).*$`), Replacement: `${1}` + RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)("(?:authorization|proxy[-_]authorization|cookie|set[-_]cookie|x[-_]api[-_]key|x[-_]auth[-_]token|x[-_]csrf[-_]token|token|access[-_]token|refresh[-_]token|password|passwd|secret|api[-_]key|client[-_]secret|private[-_]key|credential|credential[-_]ref|credential[-_]reference|secret[-_]ref|token[-_]ref|signed[-_]url)"\s*:\s*)"[^"]*"`), Replacement: `${1}"` + RedactedValue + `"`},
	{Pattern: regexp.MustCompile(`(?i)((?:authorization|proxy[-_]?authorization|cookie|set[-_]?cookie|x[-_]api[-_]key|x[-_]auth[-_]token|x[-_]csrf[-_]token|token|access[-_]?token|refresh[-_]?token|password|passwd|secret|api[-_]?key|client[-_]?secret|private[-_]?key|credential(?:[-_]?(?:ref|reference))?|secret[-_]?ref|token[-_]?ref|signed[-_]?url)\s*[=:]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`), Replacement: `${1}` + RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)([?&](?:token|access_token|refresh_token|api_key|key|signature|sig|credential|password|secret)=)[^&#\s]+`), Replacement: `${1}` + RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`), Replacement: `${1}` + RedactedValue + `@`},
	{Pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9]{20,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{16,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s]+`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)(?:/Users/|/home/|[A-Z]:\\Users\\)[^\s,;]+`), Replacement: RedactedValue},
	{Pattern: regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+[a-z]{2,}\b`), Replacement: RedactedValue},
}

var sensitiveMetadataKey = regexp.MustCompile(`(?i)^(?:authorization|proxy[-_]authorization|cookie|set[-_]cookie|x[-_]api[-_]key|x[-_]auth[-_]token|x[-_]csrf[-_]token|token|access[-_]token|refresh[-_]token|password|passwd|secret|api[-_]key|client[-_]secret|private[-_]key|credential|credential[-_]ref|credential[-_]reference|secret[-_]ref|token[-_]ref|signed[-_]url)$`)

func redactString(value string) (string, int) {
	return redact.Apply(value, redactionRules)
}
