// Package tunnelrouterules is the one definition of a tunnel route's rewrite rules: request and response header
// add/set/remove, one path-prefix rewrite and redirects. The CLI parses its flags with it, the CLI and server validate
// with it, the edge applies what it returns. Rules apply to protocol http routes only, at the edge, before the request
// goes to the carrier.
//
// Prior art: ngrok's traffic policy add-headers/remove-headers actions (~/ref/ngrok-go/internal/pb/middleware.pb.go
// Headers{Add,Remove}) and tailscale serve's `CODE:target` redirect and mount-prefix strip (~/ref/tailscale/ipn/serve.go,
// ipn/ipnlocal/serve.go parseRedirectWithCode, StripPrefix); a header named here that the edge owns (forwarded
// headers, the signed visitor assertion, connection framing, cookies, security headers) is refused, as Teleport's
// reserved-header list is (~/ref/teleport/lib/srv/app/common/header.go).
package tunnelrouterules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// Bounds, one place: a route carries at most these many rules, in at most MaximumBytes of canonical JSON.
const (
	MaximumHeaderRules     = 8 // each of request and response
	MaximumRedirects       = 4
	MaximumHeaderValue     = 4096
	MaximumHeaderName      = 64
	MaximumPath            = 512
	MaximumRedirectURL     = 2048
	MaximumBytes           = 8192
	OpAdd, OpSet, OpRemove = "add", "set", "remove"
)

// ErrInvalid wraps every refusal; the message names the rule and what to change.
var ErrInvalid = errors.New("invalid route rules")

type HeaderRule struct {
	Op    string `json:"op"`
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type PathRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Redirect struct {
	From string `json:"from"`
	Code int    `json:"code"`
	To   string `json:"to"`
}

type Rules struct {
	RequestHeaders  []HeaderRule `json:"request_headers,omitempty"`
	ResponseHeaders []HeaderRule `json:"response_headers,omitempty"`
	PathRewrite     *PathRewrite `json:"path_rewrite,omitempty"`
	Redirects       []Redirect   `json:"redirects,omitempty"`
}

func (r Rules) Empty() bool {
	return len(r.RequestHeaders) == 0 && len(r.ResponseHeaders) == 0 && r.PathRewrite == nil && len(r.Redirects) == 0
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// blockedExact are headers the edge or the transport owns; blockedPrefix are families of them.
var (
	blockedExact = map[string]bool{
		"Forwarded": true, "X-Real-Ip": true, "X-Request-Id": true, "Host": true, "Connection": true, "Proxy-Connection": true,
		"Proxy-Authenticate": true, "Proxy-Authorization": true, "Keep-Alive": true, "Te": true, "Trailer": true,
		"Transfer-Encoding": true, "Upgrade": true, "Content-Length": true, "Cookie": true, "Set-Cookie": true,
		"Authorization": true, "Paperboat-Visitor-Assertion": true, "Alt-Svc": true, "Strict-Transport-Security": true,
		"Content-Security-Policy": true, "X-Content-Type-Options": true, "X-Frame-Options": true, "Referrer-Policy": true,
		"X-Robots-Tag": true, "Location": true,
		// the bare names of the prefix families below: an origin matching on a prefix would take them for Paperboat's
		"X-Forwarded": true, "X-Paperboat": true, "Paperboat": true, "Sec-Websocket": true,
	}
	blockedPrefix = []string{"X-Forwarded-", "X-Paperboat-", "Paperboat-", "Sec-Websocket-"}
)

// HeaderBlocked reports whether a rule may not name the header (case-insensitive): the edge's own headers, the signed
// visitor assertion, connection framing, credentials and the security headers it sets.
func HeaderBlocked(name string) bool {
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	if blockedExact[canonical] {
		return true
	}
	for _, prefix := range blockedPrefix {
		if strings.HasPrefix(canonical, prefix) {
			return true
		}
	}
	return false
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > MaximumHeaderName {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func normalizeHeaderRules(list []HeaderRule, which string) ([]HeaderRule, error) {
	if len(list) > MaximumHeaderRules {
		return nil, invalid("at most %d %s header rules (got %d)", MaximumHeaderRules, which, len(list))
	}
	out := make([]HeaderRule, 0, len(list))
	for i, rule := range list {
		rule.Op = strings.ToLower(strings.TrimSpace(rule.Op))
		if rule.Op != OpAdd && rule.Op != OpSet && rule.Op != OpRemove {
			return nil, invalid("%s header rule %d: op must be add, set or remove", which, i+1)
		}
		if !validHeaderName(rule.Name) {
			return nil, invalid("%s header rule %d: %q is not a valid header name (1-%d letters, digits or -_.)", which, i+1, rule.Name, MaximumHeaderName)
		}
		rule.Name = textproto.CanonicalMIMEHeaderKey(rule.Name)
		if HeaderBlocked(rule.Name) {
			return nil, invalid("%s header rule %d: %s belongs to Paperboat, the connection or the browser and cannot be changed by a route rule", which, i+1, rule.Name)
		}
		if rule.Op == OpRemove {
			if rule.Value != "" {
				return nil, invalid("%s header rule %d: remove takes no value", which, i+1)
			}
		} else if len(rule.Value) > MaximumHeaderValue || !httpguts.ValidHeaderFieldValue(rule.Value) {
			return nil, invalid("%s header rule %d: the value must be at most %d bytes with no line breaks or control characters", which, i+1, MaximumHeaderValue)
		}
		out = append(out, rule)
	}
	return out, nil
}

// cleanPrefix is an absolute, query-free path with no dot segments; a trailing slash is dropped (the root stays "/").
func cleanPrefix(label, value string) (string, error) {
	if value == "" || len(value) > MaximumPath || value[0] != '/' || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "?#\\\r\n\x00") {
		return "", invalid("%s %q must be an absolute path starting with a single / (at most %d bytes, no query, fragment or backslash)", label, value, MaximumPath)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." || segment == "." {
			return "", invalid("%s %q must not contain . or .. segments", label, value)
		}
	}
	cleaned := path.Clean(value)
	return cleaned, nil
}

func normalizeRedirectTarget(value string) (string, error) {
	if value == "" || len(value) > MaximumRedirectURL || strings.ContainsAny(value, "\\\r\n\x00 ") {
		return "", invalid("redirect target %q must be a path or an https URL (at most %d bytes, no spaces)", value, MaximumRedirectURL)
	}
	if value[0] == '/' {
		if strings.HasPrefix(value, "//") {
			return "", invalid("redirect target %q is scheme-relative; use a path starting with a single / or a full https URL", value)
		}
		for _, segment := range strings.Split(strings.SplitN(strings.SplitN(value, "?", 2)[0], "#", 2)[0], "/") {
			if segment == ".." {
				return "", invalid("redirect target %q must not contain .. segments", value)
			}
		}
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Hostname() == "" {
		return "", invalid("redirect target %q must be a path starting with / or an https:// URL with a host and no credentials", value)
	}
	return parsed.String(), nil
}

// Normalize validates the rules and returns them in canonical form (header names in canonical case, paths cleaned,
// redirects ordered longest prefix first); the zero Rules is valid.
func Normalize(rules Rules) (Rules, error) {
	var out Rules
	var err error
	if out.RequestHeaders, err = normalizeHeaderRules(rules.RequestHeaders, "request"); err != nil {
		return Rules{}, err
	}
	if out.ResponseHeaders, err = normalizeHeaderRules(rules.ResponseHeaders, "response"); err != nil {
		return Rules{}, err
	}
	if rules.PathRewrite != nil {
		from, err := cleanPrefix("path rewrite from", rules.PathRewrite.From)
		if err != nil {
			return Rules{}, err
		}
		to, err := cleanPrefix("path rewrite to", rules.PathRewrite.To)
		if err != nil {
			return Rules{}, err
		}
		out.PathRewrite = &PathRewrite{From: from, To: to}
	}
	if len(rules.Redirects) > MaximumRedirects {
		return Rules{}, invalid("at most %d redirects (got %d)", MaximumRedirects, len(rules.Redirects))
	}
	seen := map[string]bool{}
	for i, redirect := range rules.Redirects {
		from, err := cleanPrefix("redirect from", redirect.From)
		if err != nil {
			return Rules{}, err
		}
		if seen[from] {
			return Rules{}, invalid("redirect %d: %q is redirected twice", i+1, from)
		}
		seen[from] = true
		switch redirect.Code {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		default:
			return Rules{}, invalid("redirect %d: the status must be 301, 302, 307 or 308 (got %d)", i+1, redirect.Code)
		}
		to, err := normalizeRedirectTarget(redirect.To)
		if err != nil {
			return Rules{}, err
		}
		if to[0] == '/' {
			// a target at or under its own prefix matches again: /a -> /a loops, /a -> /a/b grows /a/x into /a/b/b/x...
			if _, again := remainder(strings.SplitN(strings.SplitN(to, "?", 2)[0], "#", 2)[0], from); again {
				return Rules{}, invalid("redirect %d: %q sends %q to itself or to a path under it, which would redirect again; send it elsewhere", i+1, to, from)
			}
		}
		out.Redirects = append(out.Redirects, Redirect{From: from, Code: redirect.Code, To: to})
	}
	// longest prefix first: the edge takes the first redirect whose prefix matches
	for i := 1; i < len(out.Redirects); i++ {
		for j := i; j > 0 && len(out.Redirects[j].From) > len(out.Redirects[j-1].From); j-- {
			out.Redirects[j], out.Redirects[j-1] = out.Redirects[j-1], out.Redirects[j]
		}
	}
	if encoded, _ := json.Marshal(out); len(encoded) > MaximumBytes {
		return Rules{}, invalid("the rules are %d bytes; at most %d", len(encoded), MaximumBytes)
	}
	return out, nil
}

// Decode reads stored or wire JSON strictly and normalizes it; empty input is no rules.
func Decode(raw []byte) (Rules, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return Rules{}, nil
	}
	if len(raw) > MaximumBytes {
		return Rules{}, invalid("the rules are %d bytes; at most %d", len(raw), MaximumBytes)
	}
	var rules Rules
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return Rules{}, invalid("rules are not valid JSON: %v", err)
	}
	return Normalize(rules)
}

// Canonical is the stored form: normalized, compact JSON ("{}" for no rules).
func Canonical(rules Rules) ([]byte, error) {
	normalized, err := Normalize(rules)
	if err != nil {
		return nil, err
	}
	if normalized.Empty() {
		return []byte("{}"), nil
	}
	return json.Marshal(normalized)
}

// ParseHeaderRule reads a flag value: `set:X-Name=value`, `add:X-Name=value` or `remove:X-Name`.
func ParseHeaderRule(text string) (HeaderRule, error) {
	op, rest, ok := strings.Cut(text, ":")
	if !ok {
		return HeaderRule{}, invalid("%q is not op:Name=value (op is add, set or remove)", text)
	}
	op = strings.ToLower(strings.TrimSpace(op))
	if op == OpRemove {
		return HeaderRule{Op: op, Name: strings.TrimSpace(rest)}, nil
	}
	name, value, ok := strings.Cut(rest, "=")
	if !ok || (op != OpAdd && op != OpSet) {
		return HeaderRule{}, invalid("%q is not op:Name=value (op is add or set), or remove:Name", text)
	}
	return HeaderRule{Op: op, Name: strings.TrimSpace(name), Value: value}, nil
}

// ParsePathRewrite reads `/from -> /to`.
func ParsePathRewrite(text string) (PathRewrite, error) {
	from, to, ok := strings.Cut(text, "->")
	if !ok {
		return PathRewrite{}, invalid("%q is not '/from -> /to'", text)
	}
	return PathRewrite{From: strings.TrimSpace(from), To: strings.TrimSpace(to)}, nil
}

// ParseRedirect reads `/from -> 308:/to` or `/from -> https://example.com/to` (the status defaults to 302).
func ParseRedirect(text string) (Redirect, error) {
	from, target, ok := strings.Cut(text, "->")
	if !ok {
		return Redirect{}, invalid("%q is not '/from -> [301|302|307|308:]/to-or-https-URL'", text)
	}
	target = strings.TrimSpace(target)
	code := http.StatusFound
	if head, tail, found := strings.Cut(target, ":"); found && len(head) == 3 {
		parsed, err := strconv.Atoi(head)
		if err != nil {
			return Redirect{}, invalid("%q: %q is not a redirect status (301, 302, 307 or 308)", text, head)
		}
		code, target = parsed, strings.TrimSpace(tail)
	}
	return Redirect{From: strings.TrimSpace(from), Code: code, To: target}, nil
}

// ParseHeaderRules parses every flag value and returns the first refusal.
func ParseHeaderRules(values []string) ([]HeaderRule, error) {
	out := make([]HeaderRule, 0, len(values))
	for _, value := range values {
		rule, err := ParseHeaderRule(value)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}
