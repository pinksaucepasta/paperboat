package transactionalmail_test

import (
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail/mailtest"
)

func TestComposeBrandsEveryPartAndEscapesContent(t *testing.T) {
	c := transactionalmail.Content{
		Heading:    "Hello <b>x</b>",
		Paragraphs: []string{"A & B"},
		Details:    []transactionalmail.Detail{{Label: "Reason", Value: "<script>alert(1)</script>"}},
		Button:     &transactionalmail.Button{Label: "Open", URL: "https://console.example.test/team?a=1&b=2"},
		Why:        "You got this because you asked.",
	}
	m := transactionalmail.Compose(mailtest.Policy, "a@example.test", "Subject", c)
	mailtest.Check(t, "layout", m, "https://console.example.test/team?a=1&b=2")
	for _, bad := range []string{"<b>x</b>", "<script>", "A & B"} {
		if strings.Contains(m.HTML, bad) {
			t.Errorf("HTML holds unescaped %q", bad)
		}
	}
	for _, want := range []string{"Hello <b>x</b>", "Reason: <script>alert(1)</script>", "Open: https://console.example.test/team?a=1&b=2", "You got this because you asked."} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
}

func TestComposeWithoutLogoShowsTheWordmarkAlone(t *testing.T) {
	m := transactionalmail.Compose(config.TransactionalMail{From: "n@example.test", ConsoleOrigin: "https://console.example.test/"}, "a@example.test", "S", transactionalmail.Content{Paragraphs: []string{"Hi"}})
	if strings.Contains(m.HTML, "<img") || !strings.Contains(m.HTML, ">paperboat</a>") || strings.Contains(m.HTML, `class="btn"`) || !strings.Contains(m.Text, "Paperboat · console.example.test") {
		t.Fatalf("unexpected HTML: %s", m.HTML)
	}
}
