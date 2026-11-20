package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/privatepreviewproxy"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
)

const (
	privateAccessRoutesPath = "/v1/private-access/routes"
)

type accessorAdmission = connectorprotocol.PrivateAccessAdmission

// validateAccessorAdmission is the shared admission check plus this machine's own: the carrier
// certificate chain must parse and match its pin, and an HTTP host must be in canonical IDNA form.
func validateAccessorAdmission(a accessorAdmission, now time.Time) error {
	if a.Validate(now) != nil || !validEdgeCarrierServerCertificateChainPEM(a.EdgeCarrierServerCertificateChainPEM, a.EdgeCarrierServerSPKISHA256) || !canonicalAccessorHost(a) {
		return ErrPrivateAccessInvalid
	}
	return nil
}

func canonicalAccessorHost(a accessorAdmission) bool {
	switch a.MatchType {
	case "exact", "managed_exact":
		normalized, err := privateAccessEndpointHost("https://" + a.Hostname)
		return err == nil && normalized == a.Hostname
	case "one_label_wildcard":
		normalized, err := privateAccessEndpointHost("https://" + a.WildcardSuffix)
		return err == nil && normalized == a.WildcardSuffix && strings.Contains(a.WildcardSuffix, ".")
	}
	return true
}

type accessorSnapshot = connectorprotocol.PrivateAccessSnapshot

type accessorDiscoveryClient struct {
	endpoint *url.URL
	auth     privateAccessMachineAuth
	client   *http.Client
	// One snapshot serves every caller for accessorSnapshotReuse: each active private TCP session checks
	// its route every second, so N sessions on a machine asked the server N times a second (P3).
	mu      sync.Mutex
	fetched time.Time
	last    []accessorAdmission
	lastErr error
	now     func() time.Time
	// events, when set, makes the snapshot push-driven: see snapshot.
	events *machineevents.Subscription
}

// accessorSnapshotReuse is how long one routes snapshot answers every caller: the sessions' 1 s
// revocation check, so sharing it adds no delay to a withdrawn route ending.
const accessorSnapshotReuse = time.Second

// accessorSnapshotFallback is how long a good snapshot is trusted while the private_routes event stream is
// healthy and has not moved: pushes carry revocation, this only bounds a missed one.
const accessorSnapshotFallback = 5 * time.Minute

func newAccessorDiscoveryClient(controlURL string, auth privateAccessMachineAuth, transport http.RoundTripper) (*accessorDiscoveryClient, error) {
	u, err := url.Parse(strings.TrimSpace(controlURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrPrivateAccessInvalid
	}
	u.Path = strings.TrimRight(u.Path, "/") + privateAccessRoutesPath
	if transport == nil {
		transport = httptransport.Default()
	}
	return &accessorDiscoveryClient{endpoint: u, auth: auth, client: &http.Client{Transport: errorreport.TransportOperation(transport, u.String(), "accessor_discovery"), Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrPrivateAccessInvalid }}}, nil
}

func (c *accessorDiscoveryClient) snapshot(ctx context.Context) ([]accessorAdmission, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	pushed := false
	if c.events != nil {
		select {
		case <-c.events.Changed():
			pushed = true // consumed before the fetch, so a change during it wakes the next call
		default:
		}
	}
	reuse := accessorSnapshotReuse
	if c.events.Healthy() && c.lastErr == nil {
		reuse = accessorSnapshotFallback
	}
	if !pushed && !c.fetched.IsZero() && now().Sub(c.fetched) < reuse {
		return slices.Clone(c.last), c.lastErr
	}
	c.last, c.lastErr = c.fetchSnapshot(ctx)
	if ctx.Err() == nil {
		c.fetched = now() // a caller that gave up does not decide the others' answer
	} else {
		c.fetched = time.Time{}
	}
	return slices.Clone(c.last), c.lastErr
}

func (c *accessorDiscoveryClient) fetchSnapshot(ctx context.Context) ([]accessorAdmission, error) {
	id, err := newPrivateAccessIdentifier()
	if err != nil {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	body, _ := json.Marshal(map[string]string{"idempotency_key": "routes_" + id})
	token, err := c.auth.Token(ctx)
	if err != nil {
		return nil, privatepreviewproxy.ErrAccessAuthentication
	}
	proof, err := c.auth.Proof(ctx, "routes_"+id, http.MethodPost, c.endpoint.Path, body)
	if err != nil {
		return nil, privatepreviewproxy.ErrAccessAuthentication
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Paperboat-Machine-Identity", token)
	r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "routes_"+id)
	resp, err := c.client.Do(r)
	if err != nil {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return nil, privatepreviewproxy.ErrAccessAuthentication
	}
	if resp.StatusCode == 403 {
		return nil, privatepreviewproxy.ErrAccessForbidden
	}
	if resp.StatusCode != 200 {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	if values := resp.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, connectorprotocol.MaximumSnapshotBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > connectorprotocol.MaximumSnapshotBytes {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var out accessorSnapshot
	if d.Decode(&out) != nil || d.Decode(&struct{}{}) != io.EOF || out.ValidateEnvelope() != nil {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	seen := make(map[string]struct{}, len(out.Admissions))
	for _, admission := range out.Admissions {
		if validateAccessorAdmission(admission, serverclock.Now().UTC()) != nil {
			return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
		}
		// A route is admitted once per edge of the account's pair (primary and fallback).
		key := admission.MachineID + "\x00" + admission.ResourceID + "\x00" + admission.RouteID + "\x00" + admission.AssignmentID + "\x00" + admission.EdgeNodeID
		if _, ok := seen[key]; ok {
			return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
		}
		seen[key] = struct{}{}
	}
	return out.Admissions, nil
}
