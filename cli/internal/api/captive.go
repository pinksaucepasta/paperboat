package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CaptivePortalMessage is what pb doctor and pb status say while a Wi-Fi sign-in page blocks the network.
const CaptivePortalMessage = "A captive portal (Wi-Fi sign-in page) is blocking the network; open a browser to sign in"

const captiveProbeTimeout = 3 * time.Second

// CaptivePortal reports whether a captive portal answers in place of the Paperboat server at baseURL. Call it only
// after a request to the server failed: it asks the server's own health endpoint, https first, then plain http
// (portals intercept plain http), and says yes only for an answer that is not the server's: a redirect to another
// host, 511, or a 2xx page that is not the health answer. Any error or other status says no, so a plain outage is
// never called a portal. Prior art: tailscale net/captivedetection (probe its own endpoint, no redirects, short
// timeout, compare the answer); the endpoint here is ours, from the configured server URL. httpClient nil uses the
// process client (proxy settings included).
func CaptivePortal(ctx context.Context, baseURL string, httpClient *http.Client) bool {
	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || target.Host == "" {
		return false
	}
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	probe := *httpClient
	probe.Timeout = captiveProbeTimeout
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, scheme := range []string{"https", "http"} {
		target.Scheme = scheme
		if verdict := captiveVerdict(ctx, &probe, target.JoinPath("healthz").String(), target.Host); verdict != captiveUnknown {
			return verdict == captiveForeign
		}
	}
	return false
}

type captiveAnswer int

const (
	captiveUnknown captiveAnswer = iota // no answer, or a status a portal does not use
	captiveServer                       // the server answered
	captiveForeign                      // something else answered
)

func captiveVerdict(ctx context.Context, client *http.Client, probeURL, host string) captiveAnswer {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return captiveUnknown
	}
	response, err := client.Do(request)
	if err != nil {
		return captiveUnknown
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		// The server may send plain http to https on its own host; a portal sends the browser to its page.
		if location, err := response.Location(); err == nil && strings.EqualFold(location.Host, host) {
			return captiveUnknown
		}
		return captiveForeign
	case response.StatusCode == http.StatusNetworkAuthenticationRequired:
		return captiveForeign
	case response.StatusCode >= 200 && response.StatusCode < 300:
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if strings.Contains(string(body), `"healthy"`) {
			return captiveServer
		}
		return captiveForeign
	}
	return captiveUnknown
}
