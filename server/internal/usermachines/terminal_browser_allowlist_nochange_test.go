package usermachines

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/viewerallow"
)

// These tables were recorded from the terminal-sharing parsers before they moved to shared/viewerallow
// (normalizeVerifiedWorkOSEmail, normalizeTerminalBrowserEmailPatterns, normalizeTerminalBrowserDomain and
// normalizeTerminalBrowserEmails). The shared Terminal rules must give the same result for every input.
func TestTerminalBrowserAllowListUnchangedByViewerAllow(t *testing.T) {
	for _, test := range []struct {
		in        string
		email     string
		emailOK   bool
		pattern   string
		patternOK bool
		domain    string
		domainOK  bool
	}{
		{"", "", false, "", false, "", false},
		{" ", "", false, "", false, "", false},
		{"a", "", false, "", false, "", false},
		{"a@", "", false, "", false, "", false},
		{"@b.com", "", false, "", false, "", false},
		{"a@b", "", false, "", false, "", false},
		{"a@b.com", "a@b.com", true, "a@b.com", true, "", false},
		{"A@B.COM", "a@b.com", true, "a@b.com", true, "", false},
		{"  Ann@Example.com  ", "ann@example.com", true, "ann@example.com", true, "", false},
		{"\tann@example.com\n", "ann@example.com", true, "ann@example.com", true, "", false},
		{"ann@example.com.", "", false, "", false, "", false},
		{"ann@.example.com", "", false, "", false, "", false},
		{"ann@example..com", "", false, "", false, "", false},
		{"ann@-x.com", "", false, "", false, "", false},
		{"ann@x-.com", "", false, "", false, "", false},
		{"ann@x_y.com", "", false, "", false, "", false},
		{"ann@1.2.3.4", "ann@1.2.3.4", true, "ann@1.2.3.4", true, "", false},
		{"ann@[1.2.3.4]", "", false, "", false, "", false},
		{"ann@localhost", "", false, "", false, "", false},
		{"a?b@x.com", "", false, "", false, "", false},
		{"a*b@x.com", "", false, "", false, "", false},
		{"ann@x?.com", "", false, "", false, "", false},
		{"a b@x.com", "", false, "", false, "", false},
		{"\"ab\"@x.com", "", false, "", false, "", false},
		{"\"a b\"@x.com", "", false, "", false, "", false},
		{"a@b@c.com", "", false, "", false, "", false},
		{"ann+tag@example.com", "ann+tag@example.com", true, "ann+tag@example.com", true, "", false},
		{"a.b@example.com", "a.b@example.com", true, "a.b@example.com", true, "", false},
		{"ann@bücher.de", "ann@xn--bcher-kva.de", true, "ann@xn--bcher-kva.de", true, "", false},
		{"ann@xn--bcher-kva.de", "ann@xn--bcher-kva.de", true, "ann@xn--bcher-kva.de", true, "", false},
		{"ANN@BÜCHER.DE", "ann@xn--bcher-kva.de", true, "ann@xn--bcher-kva.de", true, "", false},
		{"jöe@x.com", "jöe@x.com", true, "jöe@x.com", true, "", false},
		{"ann@x.com\x00", "", false, "", false, "", false},
		{"ann@x\r.com", "", false, "", false, "", false},
		{"ann@x\x7f.com", "", false, "", false, "", false},
		{"ann@x:80.com", "", false, "", false, "", false},
		{"ann@x%y.com", "", false, "", false, "", false},
		{"ann@x#y.com", "", false, "", false, "", false},
		{"ann@x/y.com", "", false, "", false, "", false},
		{"ann@x\\y.com", "", false, "", false, "", false},
		{"Ann <ann@x.com>", "", false, "", false, "", false},
		{"*", "", false, "", false, "", false},
		{"*@", "", false, "", false, "", false},
		{"*@*", "", false, "", false, "", false},
		{"*@com", "", false, "", false, "", false},
		{"*@example.com", "", false, "*@example.com", true, "example.com", true},
		{"*@EXAMPLE.com", "", false, "*@example.com", true, "example.com", true},
		{" *@example.com ", "", false, "*@example.com", true, "", false},
		{"*@.example.com", "", false, "", false, "", false},
		{"*@example.com.", "", false, "", false, "", false},
		{"*@sub.example.com", "", false, "*@sub.example.com", true, "sub.example.com", true},
		{"*@bücher.de", "", false, "*@xn--bcher-kva.de", true, "xn--bcher-kva.de", true},
		{"*@1.2.3.4", "", false, "*@1.2.3.4", true, "1.2.3.4", true},
		{"*@x_y.com", "", false, "", false, "", false},
		{"**@x.com", "", false, "", false, "", false},
		{"a*@x.com", "", false, "", false, "", false},
		{"*@@x.com", "", false, "", false, "", false},
		{"*@x@y.com", "", false, "", false, "", false},
		{"*@x。com", "", false, "*@x.com", true, "x.com", true},
		{"a@x。com", "a@x.com", true, "a@x.com", true, "", false},
		{"*@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", false, "*@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", true, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", true},
		{"a@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", false, "", false, "", false},
		{"ab@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", false, "", false, "", false},
		{"*@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbc", "", false, "", false, "", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.com", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.com", true, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.com", true, "", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.co", "", false, "", false, "", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.co", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.co", true, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@x.co", true, "", false},
		{"ann@cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.com", "", false, "", false, "", false},
		{"ann@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com", "ann@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com", true, "ann@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com", true, "", false},
		{"ann@ex ample.com", "", false, "", false, "", false},
		{"an\u00a0n@x.com", "an\u00a0n@x.com", true, "an\u00a0n@x.com", true, "", false},
		{"ann@x.c", "ann@x.c", true, "ann@x.c", true, "", false},
		{"ann@x.123", "ann@x.123", true, "ann@x.123", true, "", false},
		{"ann@123", "", false, "", false, "", false},
		{"*@123.456", "", false, "*@123.456", true, "123.456", true},
		{"a\x01b@x.com", "", false, "", false, "", false},
		{"a\x7fb@x.com", "", false, "", false, "", false},
		{"a\tb@x.com", "", false, "", false, "", false},
		{"\xff@x.com", "�@x.com", true, "�@x.com", true, "", false},
		{"a@\xff.com", "", false, "", false, "", false},
		{"*@\xff.com", "", false, "", false, "", false},
		{"ÄNN@x.com", "änn@x.com", true, "änn@x.com", true, "", false},
		{"ann@x.COM ", "ann@x.com", true, "ann@x.com", true, "", false},
		{"*@x.com\t", "", false, "*@x.com", true, "x.com", true},
		{"ann@[::1]", "", false, "", false, "", false},
		{"*@::1", "", false, "", false, "", false},
		{"ann@x..", "", false, "", false, "", false},
		{"a@b.c.", "", false, "", false, "", false},
		{"*@b.c.", "", false, "", false, "", false},
		{"ann@xn--zz.com", "", false, "", false, "", false},
		{"ann@xn--.com", "", false, "", false, "", false},
		{"*@x.co_m", "", false, "", false, "", false},
		{"ann@ä.com", "ann@xn--4ca.com", true, "ann@xn--4ca.com", true, "", false},
		{"ann@ß.de", "ann@xn--zca.de", true, "ann@xn--zca.de", true, "", false},
		{"*@faß.de", "", false, "*@xn--fa-hia.de", true, "xn--fa-hia.de", true},
	} {
		email, ok := viewerallow.Terminal.Email(test.in)
		if email != test.email || ok != test.emailOK {
			t.Errorf("Terminal.Email(%q) = %q, %v; want %q, %v", test.in, email, ok, test.email, test.emailOK)
		}
		patterns, err := viewerallow.Terminal.Canonical([]string{test.in})
		pattern := ""
		if err == nil {
			pattern = patterns[0]
		}
		if pattern != test.pattern || (err == nil) != test.patternOK {
			t.Errorf("Terminal.Canonical(%q) = %q, %v; want %q, %v", test.in, pattern, err, test.pattern, test.patternOK)
		}
		// The old domain parser only ran inside "*@domain"; the Canonical row above covers it. A bare domain must
		// still parse the same way whenever the old parser accepted it.
		if test.domainOK {
			if domain, ok := viewerallow.Terminal.Domain(test.in[2:]); domain != test.domain || !ok {
				t.Errorf("Terminal.Domain(%q) = %q, %v; want %q", test.in[2:], domain, ok, test.domain)
			}
		}
	}

	big := make([]string, 101)
	for i := range big {
		big[i] = fmt.Sprintf("u%03d@x.com", i)
	}
	for _, test := range []struct {
		in         []string
		patterns   []string
		patternsOK bool
		emails     []string
		emailsOK   bool
	}{
		{[]string(nil), []string{}, true, []string{}, true},
		{[]string{}, []string{}, true, []string{}, true},
		{[]string{"b@x.com", "a@x.com"}, []string{"a@x.com", "b@x.com"}, true, []string{"a@x.com", "b@x.com"}, true},
		{[]string{"A@x.com", "a@x.com"}, []string(nil), false, []string(nil), false},
		{[]string{" a@x.com", "a@x.com"}, []string(nil), false, []string(nil), false},
		{[]string{"*@x.com", "*@X.com"}, []string(nil), false, []string(nil), false},
		{[]string{"*@bücher.de", "*@xn--bcher-kva.de"}, []string(nil), false, []string(nil), false},
		{[]string{"*@x.com", "a@x.com"}, []string{"*@x.com", "a@x.com"}, true, []string(nil), false},
		{[]string{"a@x.com", "bad"}, []string(nil), false, []string(nil), false},
		{[]string{""}, []string(nil), false, []string(nil), false},
		{big[:100], big[:100], true, big[:100], true},
		{big[:101], []string(nil), false, []string(nil), false},
	} {
		patterns, err := viewerallow.Terminal.Canonical(test.in)
		if (err == nil) != test.patternsOK || err == nil && !reflect.DeepEqual(patterns, test.patterns) {
			t.Errorf("Terminal.Canonical(%q) = %#v, %v; want %#v, ok %v", test.in, patterns, err, test.patterns, test.patternsOK)
		}
		emails, err := viewerallow.Terminal.Emails(test.in)
		if (err == nil) != test.emailsOK || err == nil && !reflect.DeepEqual(emails, test.emails) {
			t.Errorf("Terminal.Emails(%q) = %#v, %v; want %#v, ok %v", test.in, emails, err, test.emails, test.emailsOK)
		}
	}
}
