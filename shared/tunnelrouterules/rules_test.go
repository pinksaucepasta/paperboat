package tunnelrouterules

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeAcceptsAGoodRuleSetInCanonicalForm(t *testing.T) {
	rules, err := Normalize(Rules{
		RequestHeaders:  []HeaderRule{{Op: "SET", Name: "x-env", Value: "prod"}, {Op: "remove", Name: "x-debug"}},
		ResponseHeaders: []HeaderRule{{Op: "add", Name: "x-served-by", Value: "paperboat"}},
		PathRewrite:     &PathRewrite{From: "/api/", To: "/"},
		Redirects:       []Redirect{{From: "/old", Code: 301, To: "/new"}, {From: "/old/deep", Code: 308, To: "https://example.test/deep"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rules.RequestHeaders[0].Op != "set" || rules.RequestHeaders[0].Name != "X-Env" || rules.PathRewrite.From != "/api" || rules.Redirects[0].From != "/old/deep" {
		t.Fatalf("not canonical: %+v", rules)
	}
	encoded, err := Canonical(rules)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(encoded)
	if err != nil || again.PathRewrite.From != "/api" || len(again.Redirects) != 2 {
		t.Fatalf("round trip: %v %+v", err, again)
	}
	if empty, _ := Canonical(Rules{}); string(empty) != "{}" {
		t.Fatalf("no rules = %q", empty)
	}
}

func TestNormalizeRefusesWhatTheEdgeOwnsAndWhatIsMalformed(t *testing.T) {
	long := strings.Repeat("a", MaximumHeaderValue+1)
	var tooMany []HeaderRule
	for i := 0; i < MaximumHeaderRules+1; i++ {
		tooMany = append(tooMany, HeaderRule{Op: "set", Name: "X-A", Value: "1"})
	}
	for name, rules := range map[string]Rules{
		"forwarded":             {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-Forwarded-For", Value: "1.2.3.4"}}},
		"forwarded lower case":  {RequestHeaders: []HeaderRule{{Op: "set", Name: "x-forwarded-proto", Value: "http"}}},
		"real ip":               {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-Real-IP", Value: "1.2.3.4"}}},
		"visitor assertion":     {RequestHeaders: []HeaderRule{{Op: "remove", Name: "paperboat-visitor-assertion"}}},
		"paperboat family":      {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-Paperboat-Grant", Value: "x"}}},
		"cookie":                {RequestHeaders: []HeaderRule{{Op: "set", Name: "Cookie", Value: "a=b"}}},
		"set-cookie":            {ResponseHeaders: []HeaderRule{{Op: "add", Name: "Set-Cookie", Value: "a=b"}}},
		"security header":       {ResponseHeaders: []HeaderRule{{Op: "set", Name: "Strict-Transport-Security", Value: "max-age=0"}}},
		"framing":               {RequestHeaders: []HeaderRule{{Op: "set", Name: "Transfer-Encoding", Value: "chunked"}}},
		"host":                  {RequestHeaders: []HeaderRule{{Op: "set", Name: "Host", Value: "evil.test"}}},
		"bare forwarded":        {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-Forwarded", Value: "x"}}},
		"bare paperboat":        {RequestHeaders: []HeaderRule{{Op: "set", Name: "Paperboat", Value: "x"}}},
		"bare x-paperboat":      {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-Paperboat", Value: "x"}}},
		"bare websocket":        {RequestHeaders: []HeaderRule{{Op: "set", Name: "Sec-WebSocket", Value: "x"}}},
		"control character":     {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-A", Value: "a\x01b"}}},
		"redirect to itself":    {Redirects: []Redirect{{From: "/a", Code: 308, To: "/a"}}},
		"redirect under itself": {Redirects: []Redirect{{From: "/a", Code: 308, To: "/a/b?x=1"}}},
		"redirect root":         {Redirects: []Redirect{{From: "/", Code: 302, To: "/home"}}},
		"bad op":                {RequestHeaders: []HeaderRule{{Op: "replace", Name: "X-A", Value: "1"}}},
		"bad name":              {RequestHeaders: []HeaderRule{{Op: "set", Name: "X A", Value: "1"}}},
		"line break":            {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-A", Value: "1\r\nX-B: 2"}}},
		"remove with value":     {RequestHeaders: []HeaderRule{{Op: "remove", Name: "X-A", Value: "1"}}},
		"huge value":            {RequestHeaders: []HeaderRule{{Op: "set", Name: "X-A", Value: long}}},
		"too many headers":      {RequestHeaders: tooMany},
		"relative from":         {PathRewrite: &PathRewrite{From: "api", To: "/"}},
		"dot segments":          {PathRewrite: &PathRewrite{From: "/a/../b", To: "/"}},
		"scheme-relative":       {Redirects: []Redirect{{From: "/a", Code: 301, To: "//evil.test/x"}}},
		"http target":           {Redirects: []Redirect{{From: "/a", Code: 301, To: "http://example.test/x"}}},
		"credentials in url":    {Redirects: []Redirect{{From: "/a", Code: 301, To: "https://user:pw@example.test/x"}}},
		"bad status":            {Redirects: []Redirect{{From: "/a", Code: 303, To: "/x"}}},
		"duplicate redirect":    {Redirects: []Redirect{{From: "/a", Code: 301, To: "/x"}, {From: "/a/", Code: 302, To: "/y"}}},
		"too many redirects":    {Redirects: []Redirect{{From: "/1", Code: 301, To: "/x"}, {From: "/2", Code: 301, To: "/x"}, {From: "/3", Code: 301, To: "/x"}, {From: "/4", Code: 301, To: "/x"}, {From: "/5", Code: 301, To: "/x"}}},
	} {
		if _, err := Normalize(rules); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (err=%v)", name, err)
		}
	}
	if _, err := Decode([]byte(`{"request_headers":[],"bogus":1}`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown field was accepted: %v", err)
	}
}

func TestParseFlagForms(t *testing.T) {
	if rule, err := ParseHeaderRule("set:X-Name=a=b"); err != nil || rule != (HeaderRule{Op: "set", Name: "X-Name", Value: "a=b"}) {
		t.Fatalf("set: %+v %v", rule, err)
	}
	if rule, err := ParseHeaderRule("remove:X-Name"); err != nil || rule != (HeaderRule{Op: "remove", Name: "X-Name"}) {
		t.Fatalf("remove: %+v %v", rule, err)
	}
	for _, bad := range []string{"X-Name=1", "set:X-Name", "append:X-A=1"} {
		rule, err := ParseHeaderRule(bad)
		if err == nil {
			if _, err = Normalize(Rules{RequestHeaders: []HeaderRule{rule}}); err == nil {
				t.Errorf("%q was accepted", bad)
			}
		}
	}
	if rewrite, err := ParsePathRewrite("/api -> /"); err != nil || rewrite != (PathRewrite{From: "/api", To: "/"}) {
		t.Fatalf("rewrite: %+v %v", rewrite, err)
	}
	if redirect, err := ParseRedirect("/old -> 308:https://example.test/new"); err != nil || redirect != (Redirect{From: "/old", Code: 308, To: "https://example.test/new"}) {
		t.Fatalf("redirect: %+v %v", redirect, err)
	}
	if redirect, err := ParseRedirect("/old -> /new"); err != nil || redirect.Code != 302 {
		t.Fatalf("a redirect with no status: %+v %v", redirect, err)
	}
	if _, err := ParseRedirect("/old"); err == nil {
		t.Fatal("a redirect with no target was accepted")
	}
}

func TestRewritePathRedirectAndHeaders(t *testing.T) {
	rules, err := Normalize(Rules{
		PathRewrite: &PathRewrite{From: "/api", To: "/v1"},
		Redirects:   []Redirect{{From: "/old", Code: 308, To: "/new"}, {From: "/docs", Code: 301, To: "https://example.test/help"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"/api": "/v1", "/api/users": "/v1/users", "/apix": "/apix", "/other": "/other"} {
		if got := rules.RewritePath(in); got != want {
			t.Errorf("RewritePath(%q) = %q, want %q", in, got, want)
		}
	}
	root, _ := Normalize(Rules{PathRewrite: &PathRewrite{From: "/", To: "/app"}})
	if got := root.RewritePath("/x"); got != "/app/x" {
		t.Errorf("root prefix = %q", got)
	}
	strip, _ := Normalize(Rules{PathRewrite: &PathRewrite{From: "/api", To: "/"}})
	if got := strip.RewritePath("/api/users"); got != "/users" || strip.RewritePath("/api") != "/" {
		t.Errorf("strip prefix = %q %q", got, strip.RewritePath("/api"))
	}
	for _, test := range []struct {
		path, query, location string
		code                  int
		ok                    bool
	}{
		{"/old", "", "/new", 308, true}, {"/old/page", "a=1", "/new/page?a=1", 308, true}, {"/oldx", "", "", 0, false},
		{"/docs/start", "", "https://example.test/help/start", 301, true}, {"/other", "", "", 0, false},
	} {
		location, code, ok := rules.MatchRedirect(test.path, test.query)
		if location != test.location || code != test.code || ok != test.ok {
			t.Errorf("MatchRedirect(%q,%q) = %q %d %v", test.path, test.query, location, code, ok)
		}
	}
	header := http.Header{"X-Keep": {"1"}, "X-Drop": {"1"}, "X-Forwarded-For": {"198.51.100.7"}}
	ApplyHeaders(header, []HeaderRule{{Op: "set", Name: "X-Keep", Value: "2"}, {Op: "add", Name: "X-Keep", Value: "3"}, {Op: "remove", Name: "X-Drop"},
		{Op: "set", Name: "X-Forwarded-For", Value: "spoof"}, {Op: "remove", Name: "X-Forwarded-For"}})
	if got := header.Values("X-Keep"); len(got) != 2 || got[0] != "2" || got[1] != "3" || header.Get("X-Drop") != "" || header.Get("X-Forwarded-For") != "198.51.100.7" {
		t.Fatalf("headers after the rules = %v", header)
	}
}

// The query a visitor sent goes before the target's #fragment, which stays last: after it, the query would become part of the fragment.
func TestRedirectKeepsTheQueryBeforeTheFragment(t *testing.T) {
	rules, err := Normalize(Rules{Redirects: []Redirect{{From: "/old", Code: 308, To: "/new#frag"}, {From: "/q", Code: 308, To: "/x?a=1#f"}}})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/old?b=2": "/new?b=2#frag", "/q?b=2": "/x?a=1&b=2#f", "/old": "/new#frag"} {
		p, query, _ := strings.Cut(path, "?")
		if got, _, ok := rules.MatchRedirect(p, query); !ok || got != want {
			t.Errorf("%s -> %q (%v), want %q", path, got, ok, want)
		}
	}
}
