// Package mailtest checks that a message uses the shared mail layout and writes it out for review.
package mailtest

import (
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// Policy is the mail configuration every layout test renders with.
var Policy = config.TransactionalMail{From: "Paperboat <notify@mail.example.test>", ConsoleOrigin: "https://console.example.test", SiteURL: "https://site.example.test", AbuseMailbox: "abuse@mail.example.test"}

// Check fails unless m is branded in HTML and plain text, with button as its one link target (empty: no button).
// With PB_EMAIL_PREVIEW_DIR set it writes <name>.html and <name>.txt there.
func Check(t *testing.T, name string, m transactionalmail.Message, button string) {
	t.Helper()
	logo := Policy.SiteURL + "/apple-touch-icon.png"
	for _, want := range []string{"<!DOCTYPE html>", ">paperboat</a>", logo, "Paperboat · site.example.test", "prefers-color-scheme: dark", "max-width:560px"} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("%s: HTML lacks %q", name, want)
		}
	}
	if strings.Contains(m.HTML, "<script") || !strings.Contains(m.HTML, `<img src="`+logo+`" width="28" height="28" alt="Paperboat"`) {
		t.Errorf("%s: HTML has a script or no logo image", name)
	}
	if !strings.Contains(m.Text, "Paperboat · site.example.test") || strings.Contains(m.Text, "<p ") || strings.Contains(m.Text, "<a ") {
		t.Errorf("%s: text alternative lacks the footer or holds markup:\n%s", name, m.Text)
	}
	if button != "" && (!strings.Contains(m.HTML, `href="`+html.EscapeString(button)+`"`) || !strings.Contains(m.Text, button)) {
		t.Errorf("%s: button URL %q missing from HTML or text", name, button)
	}
	if button == "" && strings.Contains(m.HTML, `class="btn"`) {
		t.Errorf("%s: unexpected button", name)
	}
	if m.From != Policy.From || len(m.To) != 1 || m.Subject == "" {
		t.Errorf("%s: from=%q to=%v subject=%q", name, m.From, m.To, m.Subject)
	}
	if dir := os.Getenv("PB_EMAIL_PREVIEW_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for ext, body := range map[string]string{".html": m.HTML, ".txt": "Subject: " + m.Subject + "\n\n" + m.Text} {
			if err := os.WriteFile(filepath.Join(dir, name+ext), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
