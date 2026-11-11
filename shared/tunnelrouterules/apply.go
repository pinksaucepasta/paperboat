package tunnelrouterules

import (
	"net/http"
	"net/url"
	"strings"
)

// remainder is what follows prefix in p when p is the prefix itself or continues at a segment boundary.
func remainder(p, prefix string) (string, bool) {
	if prefix == "/" {
		return p, true
	}
	if p == prefix {
		return "", true
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix):], true
	}
	return "", false
}

func join(base, rest string) string {
	joined := strings.TrimSuffix(base, "/") + rest
	if joined == "" {
		return "/"
	}
	return joined
}

// RewritePath applies the path-prefix rewrite: /api -> /v1 turns /api and /api/x into /v1 and /v1/x, never /apix.
// A path it does not match comes back unchanged.
func (r Rules) RewritePath(p string) string {
	if r.PathRewrite == nil {
		return p
	}
	if rest, ok := remainder(p, r.PathRewrite.From); ok {
		return join(r.PathRewrite.To, rest)
	}
	return p
}

// MatchRedirect is the redirect for request path p (the longest matching prefix; Normalize orders them): the status
// and the Location, with what followed the prefix appended to the target's path and the query kept.
func (r Rules) MatchRedirect(p, rawQuery string) (location string, code int, ok bool) {
	for _, redirect := range r.Redirects {
		rest, matched := remainder(p, redirect.From)
		if !matched {
			continue
		}
		target := redirect.To
		if target[0] == '/' {
			target = join(target, rest)
		} else if parsed, err := url.Parse(target); err == nil {
			parsed.Path = join(parsed.Path, rest)
			target = parsed.String()
		}
		if rawQuery != "" {
			if strings.Contains(target, "?") {
				target += "&" + rawQuery
			} else {
				target += "?" + rawQuery
			}
		}
		return target, redirect.Code, true
	}
	return "", 0, false
}

// ApplyHeaders runs the rules in order on h: add appends, set replaces, remove deletes. A blocked header is skipped
// even though the API refuses it, so a rule can never touch what the edge sets.
func ApplyHeaders(h http.Header, rules []HeaderRule) {
	for _, rule := range rules {
		if HeaderBlocked(rule.Name) {
			continue
		}
		switch rule.Op {
		case OpAdd:
			h.Add(rule.Name, rule.Value)
		case OpSet:
			h.Set(rule.Name, rule.Value)
		case OpRemove:
			h.Del(rule.Name)
		}
	}
}
