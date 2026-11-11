package edgehttp

import (
	"context"
	"errors"
	runtimeadmission "github.com/pinksaucepasta/paperboat/shared/admission"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

const browserCallbackPath = "/.paperboat/access/callback"
const browserEdgeCookie = "__Host-pb-edge"
const browserHandoffCookie = "__Host-pb-handoff"

type BrowserAuthority interface {
	Begin(context.Context, string, string) (control.BrowserBegin, error)
	Redeem(context.Context, string, string, string, string) (control.BrowserSession, error)
	Authorize(context.Context, control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error)
}

type LazyAuthority interface {
	Activate(context.Context, control.BrowserAuthorizeRequest) (connectorprotocol.IngressDecision, error)
}

// BrowserAccess is installed only after the managed hostname isolation gate.
// Tests use isolated HTTPS origins; insecure development is not a rollout mode.
type BrowserAccess struct {
	Authority     BrowserAuthority
	LazyAuthority LazyAuthority
	LoginOrigin   string
}
type browserDecisionKey struct{}

func reservedBrowserCookie(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "__host-pb-") || strings.HasPrefix(n, "pb-dev-") || strings.HasPrefix(n, "paperboat_")
}
func stripBrowserRequestCredentials(h http.Header) {
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "paperboat-") {
			h.Del(name)
		}
	}
	var cookies []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			name, _, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && !reservedBrowserCookie(name) {
				cookies = append(cookies, strings.TrimSpace(part))
			}
		}
	}
	h.Del("Cookie")
	if len(cookies) > 0 {
		h.Set("Cookie", strings.Join(cookies, "; "))
	}
}
func stripBrowserResponseCredentials(h http.Header) {
	values := h.Values("Set-Cookie")
	h.Del("Set-Cookie")
	for _, value := range values {
		name, _, ok := strings.Cut(value, "=")
		if ok && !reservedBrowserCookie(strings.TrimSpace(name)) {
			h.Add("Set-Cookie", value)
		}
	}
	for name := range h {
		n := strings.ToLower(name)
		if strings.HasPrefix(n, "paperboat-") || strings.HasPrefix(n, "x-paperboat-") {
			h.Del(name)
		}
	}
}

func (b *BrowserAccess) validate() bool {
	if b == nil || b.Authority == nil {
		return false
	}
	u, err := url.Parse(b.LoginOrigin)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.String() == b.LoginOrigin
}

func browserCookie(r *http.Request, name string) (string, bool) {
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			count++
			value = c.Value
		}
	}
	return value, count == 1 && value != ""
}
func browserReservedDuplicates(r *http.Request) bool {
	seen := map[string]bool{}
	for _, c := range r.Cookies() {
		if strings.HasPrefix(strings.ToLower(c.Name), "__host-pb-") {
			if seen[c.Name] {
				return true
			}
			seen[c.Name] = true
		}
	}
	return false
}
func browserSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func clearBrowserHandoff(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: browserHandoffCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteNoneMode, MaxAge: -1})
}

// callback runs before route readiness or acquisition and never reaches an app.
func (b *BrowserAccess) callback(w http.ResponseWriter, r *http.Request) {
	browserSecurityHeaders(w)
	if !b.validate() {
		writeErrorPage(w, 503, "Paperboat could not check your access just now. Wait a moment and reload the page.")
		return
	}
	if r.Method != http.MethodPost || len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != b.LoginOrigin || browserReservedDuplicates(r) {
		writeErrorPage(w, 403, "This sign-in link is not valid. Open the address again to start a new sign-in.")
		return
	}
	state, ok := browserCookie(r, browserHandoffCookie)
	if !ok {
		writeErrorPage(w, 401, "This sign-in took too long. Open the address again to sign in.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 2 || len(r.PostForm["transaction_id"]) != 1 || len(r.PostForm["handoff"]) != 1 {
		writeErrorPage(w, 400, "This sign-in link is not valid. Open the address again to start a new sign-in.")
		return
	}
	transaction, nonce, found := strings.Cut(state, ".")
	if !found || transaction != r.PostForm.Get("transaction_id") {
		writeErrorPage(w, 403, "This sign-in link is not valid. Open the address again to start a new sign-in.")
		return
	}
	host, valid := dispatchHost(r.Host)
	if !valid || r.Host != host {
		writeErrorPage(w, 400, "This sign-in link is not valid. Open the address again to start a new sign-in.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	session, err := b.Authority.Redeem(ctx, transaction, nonce, r.PostForm.Get("handoff"), host)
	clearBrowserHandoff(w)
	if errors.Is(err, control.ErrBrowserDenied) {
		b.deniedPage(ctx, w, host)
		return
	}
	if err != nil {
		browserError(w, r, err)
		return
	}
	if session.Token == "" || !session.ExpiresAt.After(time.Now()) || !safeBrowserReturn(session.ReturnPath) {
		writeErrorPage(w, 503, "Paperboat could not check your access just now. Wait a moment and reload the page.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: browserEdgeCookie, Value: session.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	http.Redirect(w, r, session.ReturnPath, http.StatusSeeOther)
}

// deniedPage is the 403 for a signed-in account without access. "Use a different account" starts a fresh sign-in
// with switch=1 for the console to offer an account change; without a new transaction the page has no link.
func (b *BrowserAccess) deniedPage(ctx context.Context, w http.ResponseWriter, host string) {
	tx, err := b.Authority.Begin(ctx, host, "/")
	if err != nil || tx.TransactionID == "" || tx.State == "" || strings.Contains(tx.TransactionID, ".") || !tx.ExpiresAt.After(time.Now()) {
		writeErrorPage(w, http.StatusForbidden, deniedMessage)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: browserHandoffCookie, Value: tx.TransactionID + "." + tx.State, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteNoneMode, Expires: tx.ExpiresAt})
	writeErrorPageLink(w, http.StatusForbidden, deniedMessage, "Use a different account", b.LoginOrigin+"/.paperboat/access/login?switch=1&transaction_id="+url.QueryEscape(tx.TransactionID))
}
func safeBrowserReturn(path string) bool {
	if len(path) == 0 || len(path) > 2048 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n\x00") {
		return false
	}
	u, err := url.ParseRequestURI(path)
	return err == nil && !u.IsAbs() && u.Host == "" && u.User == nil && !strings.HasPrefix(u.Path, "//") && !strings.ContainsAny(u.Path, "\\\r\n\x00")
}
func browserNavigation(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document" && !isWebSocketUpgrade(r.Header)
}
func browserError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, control.ErrBrowserUnauthenticated):
		writeErrorPage(w, 401, signInMessage)
	case errors.Is(err, control.ErrBrowserDenied):
		writeErrorPage(w, http.StatusForbidden, deniedMessage)
	default:
		writeErrorPage(w, 503, "Paperboat could not check your access just now. Wait a moment and reload the page.")
	}
}

func lazyActivationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, control.ErrBrowserUnauthenticated):
		writeErrorPage(w, http.StatusUnauthorized, signInMessage)
	case errors.Is(err, control.ErrBrowserDenied):
		writeErrorPage(w, http.StatusForbidden, deniedMessage)
	case errors.Is(err, control.ErrLazyGenerationConflict):
		writeErrorPage(w, http.StatusConflict, "This preview was just updated. Reload the page.")
	case errors.Is(err, control.ErrLazyCapacity):
		w.Header().Set("Retry-After", "1")
		writeErrorPage(w, http.StatusTooManyRequests, "Too many previews are starting right now. Wait a moment and reload the page.")
	case errors.Is(err, control.ErrLazyForwardingFailed):
		writeErrorPage(w, http.StatusServiceUnavailable, "The preview did not receive this request. Reload the page to send it again.")
	case errors.Is(err, control.ErrLazyCooldown):
		w.Header().Set("Retry-After", "2")
		writeErrorPage(w, http.StatusServiceUnavailable, "This preview did not start. Wait two seconds and reload the page.")
	case errors.Is(err, control.ErrLazyHostOffline):
		writeErrorPage(w, http.StatusServiceUnavailable, "The machine behind this preview is offline. Turn it on, then reload the page.")
	case errors.Is(err, control.ErrLazyOriginUnavailable):
		writeErrorPage(w, http.StatusServiceUnavailable, upstreamUnavailableMessage)
	case errors.Is(err, control.ErrLazyActivationTimeout), errors.Is(err, context.DeadlineExceeded):
		writeErrorPage(w, http.StatusGatewayTimeout, "This preview took too long to start. Reload the page to try again.")
	default:
		writeErrorPage(w, http.StatusServiceUnavailable, "This preview could not start. Wait a moment and reload the page.")
	}
}

func (b *BrowserAccess) lazyAuthority() LazyAuthority {
	if b == nil {
		return nil
	}
	if b.LazyAuthority != nil {
		return b.LazyAuthority
	}
	authority, _ := b.Authority.(LazyAuthority)
	return authority
}

func (b *BrowserAccess) activateLazy(ctx context.Context, host, token string, machine bool) error {
	authority := b.lazyAuthority()
	if authority == nil {
		return control.ErrControlUnavailable
	}
	input := control.BrowserAuthorizeRequest{Token: token, Host: host, ResourceKind: "lazy_policy"}
	if machine {
		input.CredentialKind = "machine"
	}
	_, err := authority.Activate(ctx, input)
	return err
}
func (b *BrowserAccess) begin(w http.ResponseWriter, r *http.Request, host string) {
	browserSecurityHeaders(w)
	if !browserNavigation(r) {
		writeErrorPage(w, 401, signInMessage)
		return
	}
	path := r.URL.RequestURI()
	if !safeBrowserReturn(path) {
		writeErrorPage(w, 400, "This address cannot be opened after sign-in. Open the app address directly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	tx, err := b.Authority.Begin(ctx, host, path)
	if err != nil {
		browserError(w, r, err)
		return
	}
	if tx.TransactionID == "" || tx.State == "" || strings.Contains(tx.TransactionID, ".") || !tx.ExpiresAt.After(time.Now()) {
		writeErrorPage(w, 503, "Paperboat could not check your access just now. Wait a moment and reload the page.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: browserHandoffCookie, Value: tx.TransactionID + "." + tx.State, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteNoneMode, Expires: tx.ExpiresAt})
	http.Redirect(w, r, b.LoginOrigin+"/.paperboat/access/login?transaction_id="+url.QueryEscape(tx.TransactionID), http.StatusSeeOther)
}

func browserDecisionMatches(d connectorprotocol.IngressDecision, m route.RouteMatch) bool {
	r := m.Rule
	id := r.RouteID
	if id == "" {
		id = r.ID
	}
	generation := r.RouteGeneration
	if generation == 0 {
		generation = r.Revision
	}
	return d.Validate(time.Now().UTC()) == nil && d.Binding.Hostname == m.Host && d.Binding.RouteID == id && d.Binding.RouteGeneration == generation && d.Binding.AccountID == r.AccountID && ((r.ResourceKind == "preview" && d.Binding.PublicationID == r.TunnelID) || (r.ResourceKind != "preview" && d.Binding.TunnelID == r.TunnelID)) && d.Binding.Audience == r.AccessMode && (r.ViewerPolicyGeneration == 0 || d.PolicyGeneration == r.ViewerPolicyGeneration) && d.EdgeNodeID == r.Node && d.EdgeProcessEpoch == r.EdgeProcessEpoch && d.ConnectorID == r.ConnectorID && d.SessionID == r.ConnectorSessionID && d.ProcessGeneration == r.ConnectorProcessGeneration && d.ConfigGeneration == r.ConfigGeneration
}

// authorize owns one request's refresh loop. Context cancellation closes the
// transport's body and upgraded stream; permission is never cached across users.
func (b *BrowserAccess) authorize(w http.ResponseWriter, r *http.Request, m route.RouteMatch) (*http.Request, func(), bool) {
	if !b.validate() {
		writeErrorPage(w, 503, "Paperboat could not check your access just now. Wait a moment and reload the page.")
		return r, func() {}, false
	}
	if browserReservedDuplicates(r) {
		writeErrorPage(w, 401, "Your Paperboat sign-in cookie is not valid. Clear cookies for this site and open the address again.")
		return r, func() {}, false
	}
	token, ok := browserCookie(r, browserEdgeCookie)
	machine := false
	if values := r.Header.Values("Paperboat-Access-Token"); len(values) > 0 {
		if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
			writeErrorPage(w, 401, "The access credential sent with this request is not valid. Create a new one and try again.")
			return r, func() {}, false
		}
		token, ok, machine = values[0], true, true
	}
	if !ok {
		b.begin(w, r, m.Host)
		return r, func() {}, false
	}
	if !machine && crossSiteSubresource(r, m.Host) {
		writeErrorPage(w, 403, "This request came from another site and was blocked. Open the address directly in your browser.")
		return r, func() {}, false
	}
	if !machine && (r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions || isWebSocketUpgrade(r.Header)) {
		if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != "https://"+m.Host {
			writeErrorPage(w, 403, "This request came from another site and was blocked. Open the address directly in your browser.")
			return r, func() {}, false
		}
	}
	kind := m.Rule.ResourceKind
	if kind == "" {
		kind = "tunnel"
		if m.Rule.Kind == route.PreviewHTTPSWSS || string(m.Rule.Kind) == dataCarrierPreviewPrivateRouteKind {
			kind = "preview"
		}
	}
	id := m.Rule.RouteID
	if id == "" {
		id = m.Rule.ID
	}
	input := control.BrowserAuthorizeRequest{Token: token, Host: m.Host, ResourceKind: kind, ResourceID: browserResourceID(m.Rule), RouteID: id}
	if machine {
		input.CredentialKind = "machine"
	}
	lookup, stop := context.WithTimeout(r.Context(), 2*time.Second)
	d, err := b.Authority.Authorize(lookup, input)
	stop()
	if err != nil {
		browserError(w, r, err)
		return r, func() {}, false
	}
	if !browserDecisionMatches(d, m) {
		writeErrorPage(w, http.StatusNotFound, "")
		return r, func() {}, false
	}
	lease, err := runtimeadmission.Admit(r.Context(), d.ExpiresAt, nil)
	if err != nil {
		browserError(w, r, err)
		return r, func() {}, false
	}
	ctx := context.WithValue(lease.Context(), browserDecisionKey{}, d)
	input.GrantID, input.Token = d.GrantID, ""
	stopWatch := lease.Poll(func(check context.Context) error {
		next, e := b.Authority.Authorize(check, input)
		if errors.Is(e, control.ErrBrowserDenied) {
			return runtimeadmission.ErrRevoked
		}
		if e != nil {
			return e
		}
		prior := d
		prior.IssuedAt, prior.ExpiresAt, prior.DecisionID, prior.VisitorAssertion = next.IssuedAt, next.ExpiresAt, next.DecisionID, next.VisitorAssertion
		if !browserDecisionMatches(next, m) || prior != next {
			return runtimeadmission.ErrRevoked
		}
		d = next
		return nil
	})
	return r.WithContext(ctx), func() { lease.Close(); stopWatch() }, true
}

func hasBrowserDecision(ctx context.Context, m route.RouteMatch) bool {
	d, ok := ctx.Value(browserDecisionKey{}).(connectorprotocol.IngressDecision)
	return ok && browserDecisionMatches(d, m)
}

// crossSiteSubresource rejects cookie-authorized requests that another site, or
// another preview/tunnel host sharing this registrable domain, started without a
// top-level navigation. Without a public-suffix listing those hosts are
// same-site, so SameSite=Lax alone does not separate them. A request without
// Sec-Fetch-Site (old browsers, scripts) is not proven same-site: it passes only
// when its Origin, or else its Referer, names this host.
func crossSiteSubresource(r *http.Request, host string) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "same-site", "cross-site":
		return r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("Sec-Fetch-Dest") != "document" || (r.Method != http.MethodGet && r.Method != http.MethodHead)
	}
	if origin := r.Header.Values("Origin"); len(origin) > 0 {
		return len(origin) != 1 || origin[0] != "https://"+host
	}
	ref, err := url.Parse(r.Header.Get("Referer"))
	return err != nil || ref.Scheme != "https" || !strings.EqualFold(ref.Host, host)
}

// browserResourceID is the resource a browser session is bound to: the tunnel for a route that rides a
// runtime carrier (TunnelID then names the carrier), otherwise TunnelID (previews, connector tunnels).
func browserResourceID(rule route.RouteRule) string {
	if rule.ResourceID != "" {
		return rule.ResourceID
	}
	return rule.TunnelID
}
