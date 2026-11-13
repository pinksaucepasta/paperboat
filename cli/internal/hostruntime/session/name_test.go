package session

import (
	"strings"
	"testing"
)

// X-25: the server and the CLI accept any text a person types as a session name; the host used to refuse everything but
// [a-z0-9_-], so a named create was rejected on every attempt until the server gave up.
func TestValidSessionNameTakesTextAPersonTypes(t *testing.T) {
	for _, name := range []string{"build", "Zoë build 1", "x25 sp ünï", "a.b", "UPPER", "default", strings.Repeat("é", 64)} {
		if !ValidSessionName(name) {
			t.Errorf("ValidSessionName(%q) = false", name)
		}
	}
	for _, name := range []string{"", " lead", "trail ", "tab\there", "new\nline", strings.Repeat("a", 65), "bad\xffutf8"} {
		if ValidSessionName(name) {
			t.Errorf("ValidSessionName(%q) = true", name)
		}
	}
}
