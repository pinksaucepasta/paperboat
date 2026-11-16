package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-11-05: exec descriptors refused an API token with "token_scope_denied" while every other
// handler, the daemon (localapi.TokenScopeCode) and the CLI message table use "outside_token_scope", so
// `pb exec` showed a misleading connection error. One refusal, one code.
func TestTokenScopeRefusalsUseOneCode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"token_scope_denied"`) {
			t.Fatalf("%s answers a token scope refusal with token_scope_denied; use outside_token_scope", file)
		}
	}
}
