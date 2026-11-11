package api

import (
	"context"
	"net/http"
	"net/url"
)

// IPRules are a tunnel's or preview's owner IP allow and deny lists (CIDRs).
type IPRules struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// IPRules reads the lists; kind is "tunnels" or "previews".
func (c *Client) IPRules(ctx context.Context, kind, id string) (IPRules, error) {
	var out IPRules
	err := c.doTunnelRequest(ctx, http.MethodGet, ipRulesPath(kind, id), nil, &out, nil, nil)
	if err == nil {
		err = validateIPRules(out)
	}
	return out, err
}

// PutIPRules replaces both lists and returns them normalized; empty lists remove the rules.
func (c *Client) PutIPRules(ctx context.Context, kind, id string, rules IPRules) (IPRules, error) {
	if rules.Allow == nil {
		rules.Allow = []string{}
	}
	if rules.Deny == nil {
		rules.Deny = []string{}
	}
	var out IPRules
	err := c.doTunnelRequest(ctx, http.MethodPut, ipRulesPath(kind, id), rules, &out, nil, nil)
	if err == nil {
		err = validateIPRules(out)
	}
	return out, err
}

func ipRulesPath(kind, id string) string {
	return "/v1/" + kind + "/" + url.PathEscape(id) + "/ip-rules"
}

func validateIPRules(r IPRules) error {
	if len(r.Allow) > 256 || len(r.Deny) > 256 {
		return ErrUnsafeTunnelResponse
	}
	for _, entry := range append(append([]string{}, r.Allow...), r.Deny...) {
		if entry == "" || len(entry) > 64 || containsTunnelControl(entry) {
			return ErrUnsafeTunnelResponse
		}
	}
	return nil
}
