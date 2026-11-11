package main

import (
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
	"github.com/spf13/cobra"
)

// tunnelRouteRuleFlags adds the rewrite-rule flags of an http route: header add/set/remove for the request and the
// response, one path-prefix rewrite and redirects. On update each list is replaced as a whole, like --allow/--deny of
// ip-rules, and has its own --clear-* flag.
func tunnelRouteRuleFlags(command *cobra.Command, update bool) {
	command.Flags().StringArray("request-header", nil, "request header rule op:Name=value (op add or set) or remove:Name, sent to the app (repeatable, at most 8)")
	command.Flags().StringArray("response-header", nil, "response header rule op:Name=value (op add or set) or remove:Name, sent to the visitor (repeatable, at most 8)")
	command.Flags().String("rewrite-path", "", "rewrite a path prefix before the app sees it: '/from -> /to'")
	command.Flags().StringArray("redirect", nil, "redirect a path prefix: '/from -> [301|302|307|308:]/to-or-https-URL' (repeatable, at most 4)")
	if update {
		command.Flags().Bool("clear-request-headers", false, "remove every request header rule")
		command.Flags().Bool("clear-response-headers", false, "remove every response header rule")
		command.Flags().Bool("clear-rewrite-path", false, "remove the path rewrite")
		command.Flags().Bool("clear-redirects", false, "remove every redirect")
	}
}

func tunnelRouteRuleFlagsChanged(command *cobra.Command) bool {
	for _, name := range []string{"request-header", "response-header", "rewrite-path", "redirect", "clear-request-headers", "clear-response-headers", "clear-rewrite-path", "clear-redirects"} {
		if flag := command.Flags().Lookup(name); flag != nil && flag.Changed {
			return true
		}
	}
	return false
}

// applyTunnelRouteRuleFlags returns the route's rules after the flags: base (the route's current rules, nil on add) with
// each list the flags name replaced or cleared, normalized by the shared validator.
func applyTunnelRouteRuleFlags(command *cobra.Command, base *tunnelrouterules.Rules) (tunnelrouterules.Rules, error) {
	var rules tunnelrouterules.Rules
	if base != nil {
		rules = *base
	}
	flags := command.Flags()
	cleared := func(name string) bool {
		flag := flags.Lookup(name)
		return flag != nil && flag.Changed
	}
	for _, list := range []struct{ set, clear string }{{"request-header", "clear-request-headers"}, {"response-header", "clear-response-headers"}, {"rewrite-path", "clear-rewrite-path"}, {"redirect", "clear-redirects"}} {
		if cleared(list.set) && cleared(list.clear) {
			return rules, fmt.Errorf("--%s and --%s cannot be combined", list.set, list.clear)
		}
	}
	if cleared("request-header") {
		values, _ := flags.GetStringArray("request-header")
		parsed, err := tunnelrouterules.ParseHeaderRules(values)
		if err != nil {
			return rules, fmt.Errorf("--request-header: %w", err)
		}
		rules.RequestHeaders = parsed
	}
	if cleared("response-header") {
		values, _ := flags.GetStringArray("response-header")
		parsed, err := tunnelrouterules.ParseHeaderRules(values)
		if err != nil {
			return rules, fmt.Errorf("--response-header: %w", err)
		}
		rules.ResponseHeaders = parsed
	}
	if cleared("rewrite-path") {
		value, _ := flags.GetString("rewrite-path")
		parsed, err := tunnelrouterules.ParsePathRewrite(value)
		if err != nil {
			return rules, fmt.Errorf("--rewrite-path: %w", err)
		}
		rules.PathRewrite = &parsed
	}
	if cleared("redirect") {
		values, _ := flags.GetStringArray("redirect")
		rules.Redirects = nil
		for _, value := range values {
			parsed, err := tunnelrouterules.ParseRedirect(value)
			if err != nil {
				return rules, fmt.Errorf("--redirect: %w", err)
			}
			rules.Redirects = append(rules.Redirects, parsed)
		}
	}
	if cleared("clear-request-headers") {
		rules.RequestHeaders = nil
	}
	if cleared("clear-response-headers") {
		rules.ResponseHeaders = nil
	}
	if cleared("clear-rewrite-path") {
		rules.PathRewrite = nil
	}
	if cleared("clear-redirects") {
		rules.Redirects = nil
	}
	normalized, err := tunnelrouterules.Normalize(rules)
	if err != nil {
		return rules, errors.New(err.Error())
	}
	return normalized, nil
}
