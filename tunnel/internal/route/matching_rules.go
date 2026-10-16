package route

import (
	"net"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

func normalizeMatchHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "\r\n\x00 /?") {
		return "", ErrInvalid
	}
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	} else if strings.Count(host, ":") > 0 {
		return "", ErrInvalid
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || net.ParseIP(host) != nil {
		return "", ErrInvalid
	}
	if !utf8.ValidString(host) {
		return "", ErrInvalid
	}
	if !utf8.ValidString(host) {
		return "", ErrInvalid
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 || strings.Contains(ascii, "..") {
		return "", ErrInvalid
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalid
		}
	}
	return strings.ToLower(ascii), nil
}

func normalizeRequestPath(requestPath string) (string, error) {
	if requestPath == "" {
		return "/", nil
	}
	if !strings.HasPrefix(requestPath, "/") || strings.ContainsAny(requestPath, "\r\n\x00?#") || strings.Contains(requestPath, "//") {
		return "", ErrInvalid
	}
	for _, segment := range strings.Split(requestPath, "/") {
		if segment == "." || segment == ".." {
			return "", ErrInvalid
		}
	}
	if len(requestPath) > 1 {
		requestPath = strings.TrimSuffix(requestPath, "/")
	}
	return requestPath, nil
}

type routeCandidate struct {
	rule         RouteRule
	hostClass    int
	suffixLabels int
}

func routeAccepting(rule RouteRule) bool {
	switch rule.DesiredState {
	case "disabled", "deleted", "expired", "removed":
		return false
	default:
		return true
	}
}

func chooseRule(rules []RouteRule, host, requestPath string) (RouteRule, error) {
	var best routeCandidate
	found := false
	for _, rule := range rules {
		if !routeAccepting(rule) {
			continue
		}
		class, labels, ok := hostMatch(rule, host)
		if !ok || !pathMatches(rule.PathPrefix, requestPath) {
			continue
		}
		candidate := routeCandidate{rule: rule, hostClass: class, suffixLabels: labels}
		if !found {
			best, found = candidate, true
			continue
		}
		comparison := compareCandidates(candidate, best)
		if comparison > 0 {
			best = candidate
		} else if comparison == 0 {
			return RouteRule{}, ErrMatchConflict
		}
	}
	if !found {
		return RouteRule{}, ErrNoMatch
	}
	return best.rule, nil
}

func chooseHostRule(rules []RouteRule, host string) (RouteRule, bool) {
	var best routeCandidate
	found := false
	for _, rule := range rules {
		if !routeAccepting(rule) {
			continue
		}
		class, labels, ok := hostMatch(rule, host)
		if !ok {
			continue
		}
		candidate := routeCandidate{rule: rule, hostClass: class, suffixLabels: labels}
		if !found || compareHostCandidates(candidate, best) > 0 {
			best, found = candidate, true
		}
	}
	return best.rule, found
}

func hostMatch(rule RouteRule, host string) (int, int, bool) {
	switch rule.MatchType {
	case MatchExact, MatchManagedExact:
		return 3, 0, host == rule.Hostname
	case MatchOneLabelWildcard:
		if host == rule.WildcardSuffix || !strings.HasSuffix(host, "."+rule.WildcardSuffix) {
			return 0, 0, false
		}
		prefix := strings.TrimSuffix(host, "."+rule.WildcardSuffix)
		if prefix == "" || strings.Contains(prefix, ".") {
			return 0, 0, false
		}
		return 2, strings.Count(rule.WildcardSuffix, ".") + 1, true
	case MatchCatchAll:
		return 1, 0, true
	default:
		return 0, 0, false
	}
}

func compareCandidates(left, right routeCandidate) int {
	if left.hostClass != right.hostClass {
		return compareInt(left.hostClass, right.hostClass)
	}
	if left.suffixLabels != right.suffixLabels {
		return compareInt(left.suffixLabels, right.suffixLabels)
	}
	if len(left.rule.PathPrefix) != len(right.rule.PathPrefix) {
		return compareInt(len(left.rule.PathPrefix), len(right.rule.PathPrefix))
	}
	if left.rule.Priority != right.rule.Priority {
		// Lower priority numbers are preferred.
		if left.rule.Priority < right.rule.Priority {
			return 1
		}
		return -1
	}
	return 0
}

func compareHostCandidates(left, right routeCandidate) int {
	if left.hostClass != right.hostClass {
		return compareInt(left.hostClass, right.hostClass)
	}
	if left.suffixLabels != right.suffixLabels {
		return compareInt(left.suffixLabels, right.suffixLabels)
	}
	if left.rule.Priority != right.rule.Priority {
		if left.rule.Priority < right.rule.Priority {
			return 1
		}
		return -1
	}
	return 0
}

func compareInt(left, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func pathMatches(prefix, requestPath string) bool {
	if prefix == "/" {
		return true
	}
	return requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
}

func attachmentToRule(a Attachment) RouteRule {
	generation := a.ConfigGeneration
	if generation == 0 {
		generation = a.Generation
	}
	matchType := a.MatchType
	hostname := a.Host
	wildcardSuffix := a.WildcardSuffix
	if matchType == "" {
		matchType = MatchExact
	}
	if matchType == MatchOneLabelWildcard && wildcardSuffix == "" {
		wildcardSuffix = strings.TrimPrefix(hostname, "*.")
		hostname = ""
	}
	state := a.PreviewState
	return RouteRule{ID: a.ID, Revision: a.Revision, Generation: generation, Environment: a.Environment, Node: a.Node, Kind: a.Kind, MatchType: matchType, Hostname: hostname, WildcardSuffix: wildcardSuffix, PathPrefix: a.PathPrefix, Priority: a.Priority, Target: a.Target, Protocol: a.Protocol, OriginScheme: a.OriginScheme, PreserveHost: a.PreserveHost, HostOverride: a.HostOverride, DesiredState: a.DesiredState, ObservedState: state, Reason: a.PreviewReason}
}
