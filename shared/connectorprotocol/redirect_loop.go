package connectorprotocol

import (
	"net/http"
	"net/url"
	"strings"
)

// IsSelfRedirect reports whether an origin's response sends a GET or HEAD visitor back to the very
// URL it asked for on https://publicHost: the browser follows it forever. This is the app (or the
// proxy in front of it) redirecting to HTTPS when Paperboat already serves HTTPS. A response that
// sets a cookie may be a one-time "set and reload", so it does not count.
func IsSelfRedirect(method, publicHost, requestURI string, response *http.Response) bool {
	if response == nil || method != http.MethodGet && method != http.MethodHead || publicHost == "" || len(response.Header.Values("Set-Cookie")) > 0 {
		return false
	}
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return false
	}
	base, err := url.Parse("https://" + publicHost + requestURI)
	location, locationErr := url.Parse(strings.TrimSpace(response.Header.Get("Location")))
	if err != nil || locationErr != nil || response.Header.Get("Location") == "" {
		return false
	}
	target := base.ResolveReference(location)
	port := target.Port()
	return target.Scheme == "https" && strings.EqualFold(target.Hostname(), base.Hostname()) && (port == "" || port == "443") &&
		target.RequestURI() == base.RequestURI()
}
