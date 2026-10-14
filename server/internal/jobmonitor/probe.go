package jobmonitor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

var (
	errProbeConfiguration = errors.New("invalid health probe configuration")
	errProbeNoResponse    = errors.New("health probe returned no response")
	errProbeStatus        = errors.New("health endpoint returned a non-success status")
)

type probeFailure struct {
	cause  error
	status int
}

func (e probeFailure) Error() string { return "health probe failed" }

func (e probeFailure) Unwrap() error { return e.cause }

func (e probeFailure) DiagnosticStatus() int { return e.status }

// Probe checks only explicitly configured, credential-free health endpoints.
// It returns a static-prose error that preserves the underlying safe cause for
// the caller's bounded local observation. Bodies and addresses are never
// formatted. Redirects cannot turn a health probe into another request.
func Probe(ctx context.Context, rawURL string, timeout time.Duration) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return probeFailure{cause: err}
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "/readyz" && u.Path != "/healthz") || timeout < time.Millisecond || timeout > 10*time.Second {
		return probeFailure{cause: errProbeConfiguration}
	}
	if u.Scheme != "https" {
		ip, err := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || err != nil || !(ip.IsLoopback() || ip.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) {
			return probeFailure{cause: errProbeConfiguration}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return probeFailure{cause: err}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return probeFailure{cause: err}
	}
	if response == nil {
		return probeFailure{cause: errProbeNoResponse}
	}
	return probeResponse(response)
}

func probeResponse(response *http.Response) error {
	if response.Body == nil {
		return probeFailure{cause: errProbeNoResponse, status: response.StatusCode}
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4097))
	closeErr := response.Body.Close()
	if response.StatusCode == http.StatusOK && readErr == nil && closeErr == nil {
		return nil
	}
	causes := make([]error, 0, 3)
	if response.StatusCode != http.StatusOK {
		causes = append(causes, errProbeStatus)
	}
	if readErr != nil {
		causes = append(causes, readErr)
	}
	if closeErr != nil {
		causes = append(causes, closeErr)
	}
	return probeFailure{cause: errors.Join(causes...), status: response.StatusCode}
}
