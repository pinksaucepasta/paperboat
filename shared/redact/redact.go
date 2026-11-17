// Package redact applies an ordered table of regular-expression redaction rules to free text. Each binary keeps its own
// rule table (what is secret differs between a support bundle, telemetry and logs); this package owns only the rule
// type and how a table is applied.
package redact

import "regexp"

// Rule replaces every match of Pattern with Replacement. Replacement uses regexp expansion syntax, so ${1} keeps a
// captured prefix such as "token=".
type Rule struct {
	Pattern     *regexp.Regexp
	Replacement string
}

// Apply runs the rules in order and returns the redacted text and the number of matches replaced. Later rules see the
// output of earlier ones, so tables put the most specific rule (a private-key block) before the general ones.
func Apply(value string, rules []Rule) (string, int) {
	redactions := 0
	for _, rule := range rules {
		matches := rule.Pattern.FindAllStringIndex(value, -1)
		if len(matches) == 0 {
			continue
		}
		redactions += len(matches)
		value = rule.Pattern.ReplaceAllString(value, rule.Replacement)
	}
	return value, redactions
}
