package edgehttp

import (
	"bufio"
	"context"
	"crypto/sha1" // #nosec G505 -- required by RFC 6455 for the WebSocket handshake.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/admission"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
	edgetelemetry "github.com/pinksaucepasta/paperboat/tunnel/internal/telemetry"
	"github.com/realclientip/realclientip-go"
	"golang.org/x/net/http/httpguts"
)

// RouteMatcher is the live edge route boundary. Implementations must return
// a generation-fenced match and a lease that remains valid for the request's
// lifetime.
type RouteMatcher interface {
	Match(string, string) (route.RouteMatch, error)
	Acquire(context.Context, string, string) (*route.StreamLease, route.RouteMatch, error)
}

type Config struct {
	// OnFailure observes final proxy failures; the caller owns hosted exception policy.
	OnFailure       func(context.Context, error)
	SelfHosted      bool
	BrowserAccess   *BrowserAccess
	BrowserTerminal interface {
		Admit(context.Context, string, string, string) (control.BrowserTerminalAdmission, error)
	}
	BrowserTerminalHub       *BrowserTerminalHub
	BrowserTerminalEdgeHost  string
	InspectorAccess          *InspectorEdgeAccess
	PreviewBaseDomain        string
	TunnelBaseDomain         string
	RuntimeCarrierTransport  http.RoundTripper
	RuntimeBaseDomain        string
	TrustedProxies           []*net.IPNet
	MaxHeaderBytes           int64
	MaxBodyBytes             int64
	BodyIdleTimeout          time.Duration // cut an upload after this long without body bytes; zero never
	Routes                   RouteMatcher
	PrivateAccessConnections *PrivateAccessConnectionRegistry
	RequestID                func() string
	Readiness                interface {
		RouteState(string) (string, string, string, bool)
	}
	// PreviewPending, when set, reports a preview host the server has admitted on this edge but
	// whose carrier route is not attached yet; its requests wait for it instead of a 404.
	PreviewPending func(host string) bool
	// PreviewAdmitted, when set, reports a preview host the edge's held ingress snapshot still admits. Without its
	// carrier the request waits like a pending one and then gets 503 preview_reconnecting (never a 404).
	PreviewAdmitted func(host string) bool
	// RouteUnavailable, when set, reports a host whose route is known but failed its probe: it gets the
	// "could not reach the app" page (502) instead of a 404.
	RouteUnavailable func(host string) bool
	// RouteGrace, when set, is how long a request for an unknown host waits for its route before the
	// 404. It is non-zero only just after an install handover took over accepting: hosts re-attach
	// their carriers to the replacement after that, and a request in that gap has no route yet.
	RouteGrace   func() time.Duration
	HelperAccess interface {
		VerifyHelperAccess(context.Context, string) (admission.Claims, error)
	}
	Revocations interface {
		Revoked(context.Context, admission.Claims) (bool, error)
	}
	RevocationCheckInterval time.Duration
	// AddressAllowed, when set, applies the owner's IP allow/deny lists for a host to the client address.
	AddressAllowed func(host, address string) bool
}

type routeMatchContextKey struct{}

// RouteMatchFromContext returns the generation-fenced match selected by the
// edge policy, if this request passed through a configured route matcher.
func RouteMatchFromContext(ctx context.Context) (route.RouteMatch, bool) {
	if ctx == nil {
		return route.RouteMatch{}, false
	}
	match, ok := ctx.Value(routeMatchContextKey{}).(route.RouteMatch)
	return match, ok
}

type Policy struct {
	config         Config
	next           http.Handler
	clientStrategy realclientip.Strategy
	// routeHolds bounds the requests held for their route during the takeover grace (holdForRoute).
	routeHolds *holdSet
	// previewWaits bounds the requests that wait for an admitted preview's route (awaitPreviewRoute); past it a
	// request gets its 503 at once instead of waiting.
	previewWaits *holdSet
	// addresses is the edge-wide per-client-address limit (ip_limits.go).
	addresses *addressLimiter
}

// routeHoldMax and routeHoldPerName bound the HTTP-layer holds: 256 requests at once, 64 per host. A request got
// here past TLS, so its name is already authorized; the bound is about volume, not names.
const (
	routeHoldMax     = 256
	routeHoldPerName = 64
	// previewWaitMax and previewWaitPerName bound the requests waiting previewStartingWait for a preview route.
	previewWaitMax     = 256
	previewWaitPerName = 16
)

func New(config Config, next http.Handler) (*Policy, error) {
	if next == nil || (!config.SelfHosted || config.BrowserAccess != nil) && (config.PreviewBaseDomain == "" || config.TunnelBaseDomain == "" || config.RuntimeBaseDomain == "") || config.MaxHeaderBytes < 1024 || config.MaxBodyBytes < 1 {
		return nil, http.ErrNotSupported
	}
	trusted := make([]net.IPNet, 0, len(config.TrustedProxies))
	for _, network := range config.TrustedProxies {
		if network == nil {
			return nil, http.ErrNotSupported
		}
		trusted = append(trusted, *network)
	}
	strategy, err := realclientip.NewRightmostTrustedRangeStrategy("X-Forwarded-For", trusted)
	if err != nil {
		return nil, err
	}
	return &Policy{config: config, next: next, clientStrategy: strategy, routeHolds: newHoldSet(routeHoldMax, routeHoldPerName, 0), previewWaits: newHoldSet(previewWaitMax, previewWaitPerName, 0), addresses: newAddressLimiter(addressRequestRate, addressUpgradeRate, addressStreams)}, nil
}

// NewGateway builds the live public-preview gate. The target is the private
// public virtual host; this handler never contacts the control plane or carries policy
// data outside the edge process.
func NewGateway(config Config, privateUpstream string) (*Policy, error) {
	return NewGatewayWithTransport(config, privateUpstream, nil)
}

func NewGatewayWithTransport(config Config, privateUpstream string, previewTransport http.RoundTripper) (*Policy, error) {
	return NewGatewayWithTransports(config, privateUpstream, previewTransport, nil)
}

// NewGatewayWithTransports installs the route-kind-specific forwarding
// boundaries. Durable connector-v1 routes use canonicalTransport, while
// preview routes use previewTransport and retained helper routes use the
// private legacy upstream. A matched durable route is never silently sent to
// the legacy or preview transport when its canonical transport is absent.
func NewGatewayWithTransports(config Config, privateUpstream string, previewTransport, canonicalTransport http.RoundTripper) (*Policy, error) {
	target, err := url.Parse("http://" + privateUpstream)
	if err != nil {
		return nil, err
	}
	legacy := httputil.NewSingleHostReverseProxy(target)
	legacy.FlushInterval = -1
	configureProxyDiagnostics(legacy, config.OnFailure)
	legacy.ModifyResponse = func(response *http.Response) error {
		stripBrowserResponseCredentials(response.Header)
		if response.Request != nil {
			if app, ok := response.Request.Context().Value(appRouteKey{}).(appRoute); ok {
				appResponseHeaders(response.Header, app.gated, app.preview)
				return nil
			}
		}
		response.Header.Del("X-Robots-Tag") // the edge set it on the writer before proxying
		return nil
	}
	// originResponse guards the routes whose origin is the user's app: a self-redirect becomes the
	// loop page instead of reaching the browser.
	originResponse := func(response *http.Response) error {
		request := response.Request
		if request != nil && connectorprotocol.IsSelfRedirect(request.Method, request.Header.Get("X-Forwarded-Host"), request.URL.RequestURI(), response) {
			return errOriginRedirectLoop
		}
		if code := response.Header.Get(connectorprotocol.OriginFailureHeader); code != "" {
			return originFailure(code)
		}
		if request != nil {
			// the route's response-header rules; the edge's own headers are set after, in ModifyResponse
			if match, ok := RouteMatchFromContext(request.Context()); ok && match.Rule.RewriteRules != nil && match.Rule.Kind == route.TunnelHTTPSWSS {
				tunnelrouterules.ApplyHeaders(response.Header, match.Rule.RewriteRules.ResponseHeaders)
			}
		}
		return legacy.ModifyResponse(response)
	}
	var canonical http.Handler
	if canonicalTransport != nil {
		proxy := &httputil.ReverseProxy{
			Transport:      canonicalTransport,
			FlushInterval:  -1,
			Rewrite:        rewriteToCarrier,
			ModifyResponse: originResponse,
		}
		configureProxyDiagnostics(proxy, config.OnFailure)
		canonical = proxy
	}
	var legacyHandler http.Handler = legacy
	if privateUpstream == "" {
		legacyHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeErrorPage(w, http.StatusNotFound, "") })
	}
	var next http.Handler = legacyHandler
	if previewTransport != nil || canonicalTransport != nil || config.RuntimeCarrierTransport != nil {
		var runtimeHandler http.Handler
		if config.RuntimeCarrierTransport != nil {
			proxy := &httputil.ReverseProxy{Transport: config.RuntimeCarrierTransport, FlushInterval: -1, Rewrite: rewriteToCarrier, ModifyResponse: func(response *http.Response) error {
				if response.Request != nil && response.Request.URL.Path == "/v1/browser-terminal" {
					edgetelemetry.Log(response.Request.Context(), edgetelemetry.SeverityInfo, "browser_terminal_host_response", edgetelemetry.OutcomeSuccess, "status", response.StatusCode)
				}
				return legacy.ModifyResponse(response)
			}}
			configureProxyDiagnostics(proxy, config.OnFailure)
			runtimeHandler = proxy
		}
		var preview http.Handler
		if previewTransport != nil {
			proxy := &httputil.ReverseProxy{
				Transport:      retryPreviewTransport{next: previewTransport},
				FlushInterval:  -1,
				Rewrite:        rewriteToCarrier,
				ModifyResponse: originResponse,
			}
			configureProxyDiagnostics(proxy, config.OnFailure)
			preview = proxy
		}
		next = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if match, ok := RouteMatchFromContext(request.Context()); ok {
				switch match.Rule.Kind {
				case route.Kind("runtime_https_wss"):
					if runtimeHandler != nil {
						runtimeHandler.ServeHTTP(writer, request)
						return
					}
					writeErrorPage(writer, http.StatusNotFound, "")
					return
				case route.TunnelHTTPSWSS:
					if canonical == nil {
						writeErrorPage(writer, http.StatusNotFound, "")
						return
					}
					canonical.ServeHTTP(writer, request)
					return
				case route.PreviewHTTPSWSS, route.Kind(dataCarrierPreviewPrivateRouteKind):
					if preview != nil {
						preview.ServeHTTP(writer, request)
						return
					}
				}
			}
			if preview != nil && strings.HasSuffix(strings.ToLower(request.Host), "."+strings.ToLower(config.PreviewBaseDomain)) {
				preview.ServeHTTP(writer, request)
				return
			}
			legacyHandler.ServeHTTP(writer, request)
		})
	}
	return New(config, next)
}

type appRouteKey struct{}

type appRoute struct{ gated, preview bool }

// setVisitorAssertion tells the app behind a gated route who is visiting, signed by the server; it
// runs after stripPrivate dropped any copy the visitor sent. Public routes never carry one.
func setVisitorAssertion(r *http.Request) {
	if decision, ok := r.Context().Value(browserDecisionKey{}).(connectorprotocol.IngressDecision); ok && decision.VisitorAssertion != "" {
		r.Header.Set("Paperboat-Visitor-Assertion", decision.VisitorAssertion)
	}
}

func isAppRouteKind(kind string) bool {
	return kind == string(route.TunnelHTTPSWSS) || kind == string(route.PreviewHTTPSWSS) || kind == dataCarrierPreviewPrivateRouteKind
}

func setStrictEdgeHeaders(header http.Header) {
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
}

// appResponseHeaders applies the edge's headers to an app's response. A gated (team or private) route
// keeps the strict set: framing or leaking its URL would expose the edge's session. A public route
// keeps the app's own framing and referrer choices (Grafana panels, embedded docs) and gets the
// strict ones only where it set none. Previews are never indexed; a tunnel on a real domain is the
// owner's site. The origin's Alt-Svc describes the origin, not the edge, so it never passes.
func appResponseHeaders(header http.Header, gated, preview bool) {
	header.Del("Alt-Svc")
	if gated {
		setStrictEdgeHeaders(header)
		return
	}
	if header.Get("Referrer-Policy") == "" {
		header.Set("Referrer-Policy", "no-referrer")
	}
	if header.Get("X-Frame-Options") == "" && !strings.Contains(strings.ToLower(strings.Join(header.Values("Content-Security-Policy"), ";")), "frame-ancestors") {
		header.Set("X-Frame-Options", "DENY")
	}
	if preview {
		header.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	}
}

// rewriteToCarrier hands the origin what the edge knows and the origin cannot: the visitor used
// https on this public host, from this client address. ReverseProxy drops Forwarded and
// X-Forwarded-For/-Host/-Proto from the outgoing request before Rewrite runs, so they are copied
// back from the inbound request, where Policy already replaced the visitor's copies with its own.
// Set, never append: a second X-Forwarded-Host breaks Next.js and a leftmost X-Forwarded-For is forgeable.
func rewriteToCarrier(request *httputil.ProxyRequest) {
	request.Out.URL.Scheme = "http"
	request.Out.URL.Host = request.In.Host
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-IP"} {
		if value := request.In.Header.Get(name); value != "" {
			request.Out.Header.Set(name, value)
		}
	}
}

var errProxyHTTPDiagnostic = errors.New("edge proxy HTTP diagnostic")

type proxyDiagnosticWriter struct{ observe func(context.Context, error) }

func (w proxyDiagnosticWriter) Write(p []byte) (int, error) {
	if w.observe != nil {
		w.observe(context.Background(), errProxyHTTPDiagnostic)
	}
	return len(p), nil
}
func configureProxyDiagnostics(proxy *httputil.ReverseProxy, observe func(context.Context, error)) {
	proxy.ErrorLog = log.New(proxyDiagnosticWriter{observe: observe}, "", 0)
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
		if err == http.ErrAbortHandler {
			return
		}
		if observe != nil {
			observe(request.Context(), err)
		}
		if errors.Is(err, connectorprotocol.ErrPublicationBlocked) {
			var blocked connectorprotocol.PublicationBlockedError
			errors.As(err, &blocked)
			writeBlockedRefusal(writer, request, blocked.Legal)
			return
		}
		if errors.Is(err, ErrIngressQuotaExhausted) {
			// A typed refusal at admission: the visitor learns why, and direct access is unaffected.
			var quota connectorprotocol.IngressQuotaError
			errors.As(err, &quota)
			writeQuotaRefusal(writer, request, quota.ResetsAt, time.Now())
			return
		}
		if errors.Is(err, datacarrier.ErrStreamLimit) {
			// The machine's carrier stayed full for its whole permit wait: busy, not broken.
			writeRefusal(writer, request, http.StatusServiceUnavailable, "carrier_stream_limit", busyTitle, busyMessage, 2*time.Second)
			return
		}
		if errors.Is(err, errRequestBodyIdle) {
			writer.Header().Set("X-Paperboat-Error", "request_body_timeout")
			writeErrorPage(writer, http.StatusRequestTimeout, requestBodyIdleMessage)
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// A chunked or unknown-length body ran past the cap mid-stream: the same answer as a declared one.
			writer.Header().Set("X-Paperboat-Error", "request_body_too_large")
			writeErrorPage(writer, http.StatusRequestEntityTooLarge, "")
			return
		}
		if errors.Is(err, ErrDataCarrierPreviewResponseHeaderTimeout) {
			writer.Header().Set("X-Paperboat-Error", "origin_timeout")
			writeErrorPage(writer, http.StatusGatewayTimeout, originTimeoutMessage)
			return
		}
		var failure originFailure
		if errors.As(err, &failure) {
			status, message := failure.page()
			writer.Header().Set("X-Paperboat-Error", "origin_"+string(failure))
			writeErrorPage(writer, status, message)
			return
		}
		if errors.Is(err, errOriginRedirectLoop) {
			writer.Header().Set("X-Paperboat-Error", "origin_redirect_loop")
			writeErrorPage(writer, http.StatusBadGateway, originRedirectLoopMessage)
			return
		}
		// A visitor hitting an address whose app is not running should learn that, not just "Bad Gateway".
		// The reason is deliberately generic: it never carries the underlying cause.
		writer.Header().Set("X-Paperboat-Error", "upstream_unavailable")
		writeErrorPage(writer, http.StatusBadGateway, upstreamUnavailableMessage)
	}
}

const originTimeoutMessage = "The app took longer than 100 seconds to start answering, so Paperboat ended the request. A slow page should stream its response or report progress."

// originFailure is the agent's origin-health code for an origin it could not reach (the
// X-Paperboat-Origin-Failure header of its 502), so the visitor learns which failure it is.
type originFailure string

func (f originFailure) Error() string { return "origin failed: " + string(f) }

func (f originFailure) page() (int, string) {
	switch f {
	case connectorprotocol.OriginRefused:
		return http.StatusBadGateway, "Nothing is listening on this app's port on its machine. Its owner can start the app or fix the route's port."
	case connectorprotocol.OriginTimeout:
		return http.StatusGatewayTimeout, "The app's machine could not connect to it in time. Its owner can check that the app is healthy."
	case connectorprotocol.OriginTLS:
		return http.StatusBadGateway, "The app's TLS certificate was not accepted. Its owner can pin it (pb tunnel route update <tunnel> <route> --tls-verification pinned), give its CA, or point the route at http://."
	case connectorprotocol.OriginCertificateChanged:
		return http.StatusBadGateway, "The app presented a different certificate from the one pinned for it, so Paperboat refused the connection. If its owner replaced it, they can re-pin the route."
	}
	return http.StatusBadGateway, "The app's machine cannot reach the app's address. Its owner can check the route's origin."
}

// errOriginRedirectLoop: the origin sent the visitor back to the URL it asked for (IsSelfRedirect).
var errOriginRedirectLoop = errors.New("origin redirects to the URL it was asked for")

const originRedirectLoopMessage = "This app redirects to HTTPS, but Paperboat already serves it over HTTPS, so the browser would loop forever. " +
	"Its owner can point the route at the app's HTTPS port (for example https://localhost:443) or turn off the app's own HTTP-to-HTTPS redirect."

const previewStartingMessage = "This preview is starting. Retry in a moment."
const previewReconnectingMessage = "This app lost its connection to Paperboat for a moment and is reconnecting. Retry in a few seconds."
const requestBodyIdleMessage = "The upload stopped sending data, so Paperboat ended the request. Check your connection and try again."

// previewStartingWait bounds how long a request to an admitted preview waits for its route.
const previewStartingWait = 3 * time.Second

// awaitPreviewRoute rechecks an admitted preview's route until it attaches, the request ends or
// previewStartingWait passes.
func (p *Policy) awaitPreviewRoute(ctx context.Context, host string) (string, string, string, bool) {
	deadline := time.NewTimer(previewStartingWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", "", false
		case <-deadline.C:
			return "", "", "", false
		case <-tick.C:
			if kind, state, reason, found := p.config.Readiness.RouteState(host); found {
				return kind, state, reason, true
			}
		}
	}
}

// The visitor never learns which limit tripped or who set it; the owner reads X-Paperboat-Error.
const (
	busyTitle              = "This link is busy"
	busyMessage            = "This link is busy right now. Wait a moment and try again."
	edgeRateLimitedMessage = busyMessage
)

// writeIPRestricted answers a client address the owner's IP lists refuse; the visitor is not told
// which list or entry matched.
func writeIPRestricted(w http.ResponseWriter, r *http.Request) {
	writeRefusal(w, r, http.StatusForbidden, "ip_restricted", "Access restricted", "Access to this link is restricted.", 0)
}

// writeBlockedRefusal answers a hostname an operator took down: the fixed page, never the origin. A
// legal takedown is 451 (RFC 7725), any other 403.
func writeBlockedRefusal(w http.ResponseWriter, r *http.Request, legal bool) {
	status := http.StatusForbidden
	if legal {
		status = http.StatusUnavailableForLegalReasons
	}
	writeRefusal(w, r, status, "publication_blocked", "This link was disabled", "Paperboat disabled this address because it violated Paperboat's acceptable use policy.", 0)
}

// writeQuotaRefusal answers a workspace out of bandwidth allowance: 429 with Retry-After until the
// allowance resets (when the server said when) and the reset date on the page.
func writeQuotaRefusal(w http.ResponseWriter, r *http.Request, resetsAt, now time.Time) {
	message := "This address has used up its bandwidth allowance. Its owner can add bandwidth, or it resumes when the allowance resets."
	var wait time.Duration
	if resetsAt.After(now) {
		wait = resetsAt.Sub(now)
		message = "This address has used up its bandwidth allowance. Its owner can add bandwidth, or it resumes on " + resetsAt.UTC().Format("2 Jan 2006 15:04") + " UTC."
	}
	writeRefusal(w, r, http.StatusTooManyRequests, "bandwidth_quota_exhausted", "Bandwidth allowance used up", message, wait)
}

const upstreamUnavailableMessage = "Paperboat could not reach the app behind this address. If you own the app, start it and try again."

type retryPreviewTransport struct{ next http.RoundTripper }

func (t retryPreviewTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if _, restricted := request.Context().Value(browserDecisionKey{}).(connectorprotocol.IngressDecision); restricted {
		return t.next.RoundTrip(request)
	}
	if _, private := request.Context().Value(privateAccessRequestContextKey{}).(connectorprotocol.PrivateAccessRequest); private {
		return t.next.RoundTrip(request)
	}
	var response *http.Response
	recovery := reattach.New(reattach.Policy{Attempts: 2, Retryable: func(err error) bool {
		return retryablePreviewRequest(request) && retryablePreviewTransportError(err)
	}})
	err := recovery.Run(request.Context(), func(ctx context.Context) error {
		var err error
		response, err = t.next.RoundTrip(request.Clone(ctx))
		return err
	})
	return response, err
}

func retryablePreviewRequest(request *http.Request) bool {
	if request == nil || request.Body != nil && request.Body != http.NoBody || isWebSocketUpgrade(request.Header) {
		return false
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func retryablePreviewTransportError(err error) bool {
	if errors.Is(err, ErrDataCarrierPreviewResponseHeaderTimeout) {
		return false // a slow app already used its whole first-byte budget; asking again doubles the visitor's wait
	}
	if errors.Is(err, errPreviewRouteReplaced) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func ParseTrustedProxies(values []string) ([]*net.IPNet, error) {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, err
		}
		result = append(result, network)
	}
	return result, nil
}

func (p *Policy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	release, admitted := p.admitAddress(w, r)
	if !admitted {
		return
	}
	defer release()
	// Tickets use the selected edge's own TLS hostname so browser DNS caches
	// cannot send a fresh admission to the former active edge. The path names
	// only a candidate route; the one-use ticket and current node/epoch are
	// still checked before any terminal stream is opened.
	if p.config.BrowserTerminalEdgeHost != "" && r.Host == p.config.BrowserTerminalEdgeHost && (strings.HasPrefix(r.URL.Path, "/v1/browser-terminal/") || strings.HasPrefix(r.URL.Path, "/v1/runtime/")) {
		browserTerminal := strings.HasPrefix(r.URL.Path, "/v1/browser-terminal/")
		prefix := "/v1/runtime/"
		if browserTerminal {
			prefix = "/v1/browser-terminal/"
		}
		runtimeHost, suffix, hasSuffix := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
		_, kind, allowed := p.allowedHost(runtimeHost)
		if !allowed || kind != "runtime_https_wss" || !normalizeRequestTarget(r, p.config.BrowserTerminalEdgeHost) || (browserTerminal && (hasSuffix || r.Method != http.MethodGet || !isWebSocketUpgrade(r.Header) || r.URL.RawQuery != "")) || (!hasSuffix && (r.Method != http.MethodGet || !isWebSocketUpgrade(r.Header) || r.URL.RawQuery != "")) {
			writeErrorPage(w, http.StatusNotFound, "")
			return
		}
		r = r.Clone(r.Context())
		r.Host = runtimeHost
		if browserTerminal {
			r.URL.Path = "/v1/browser-terminal"
		} else if hasSuffix {
			r.URL.Path = "/" + suffix
		} else {
			r.URL.Path = "/v1/runtime"
		}

		r.URL.RawPath = ""
	}
	if strings.HasPrefix(r.URL.Path, inspectorEdgePrefix) {
		if p.config.InspectorAccess == nil {
			writeErrorPage(w, http.StatusNotFound, "")
		} else {
			p.config.InspectorAccess.ServeHTTP(w, r)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/.paperboat/access/") {
		if r.URL.Path == browserCallbackPath && p.config.BrowserAccess != nil {
			p.config.BrowserAccess.callback(w, r)
		} else {
			writeErrorPage(w, http.StatusNotFound, "")
		}
		return
	}
	host, expectedKind, ok := p.allowedHost(r.Host)
	var matched route.RouteMatch
	var lease *route.StreamLease
	if p.config.Routes != nil {
		ok = false
		candidateHost, valid := dispatchHost(r.Host)
		if !valid {
			writeErrorPage(w, http.StatusNotFound, "")
			return
		}
		candidateLease, acquired, acquireErr := p.config.Routes.Acquire(r.Context(), candidateHost, r.URL.Path)
		if errors.Is(acquireErr, route.ErrNoMatch) && p.config.RouteGrace != nil {
			// Past the bound a request is answered like one with no grace: its not-found below.
			if release, ok := p.routeHolds.acquire(candidateHost, true); ok {
				candidateLease, acquired, acquireErr = holdForRoute(r.Context(), p.config.RouteGrace(), func() (*route.StreamLease, route.RouteMatch, error) {
					return p.config.Routes.Acquire(r.Context(), candidateHost, r.URL.Path)
				})
				release()
			}
		}
		if acquireErr != nil {
			token, hasSession := browserCookie(r, browserEdgeCookie)
			machine := false
			if values := r.Header.Values("Paperboat-Access-Token"); len(values) == 1 && values[0] != "" && strings.TrimSpace(values[0]) == values[0] {
				token, hasSession, machine = values[0], true, true
			}
			if errors.Is(acquireErr, route.ErrNoMatch) && hasSession && !browserReservedDuplicates(r) && p.config.BrowserAccess != nil && lazyPreviewHostname(r.Host, candidateHost, p.config.PreviewBaseDomain) {
				activate, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				activationErr := p.config.BrowserAccess.activateLazy(activate, candidateHost, token, machine)
				cancel()
				if activationErr != nil {
					lazyActivationError(w, r, activationErr)
					return
				}
				candidateLease, acquired, acquireErr = p.config.Routes.Acquire(r.Context(), candidateHost, r.URL.Path)
				if acquireErr == nil {
					matched, lease = acquired, candidateLease
					goto acquiredRoute
				}
			}
			if !hasSession && r.Header.Get("Paperboat-Access-Token") == "" && p.config.BrowserAccess != nil && p.config.BrowserAccess.validate() && browserNavigation(r) {
				p.config.BrowserAccess.begin(w, r, candidateHost)
				return
			}
			// The held ingress snapshot still admits this preview but its carrier is gone: an app that is
			// reconnecting, not a name that does not exist (ngrok-go reconnecting.go answers ErrSessionNotReady).
			if errors.Is(acquireErr, route.ErrNoMatch) && p.config.PreviewAdmitted != nil && p.config.PreviewAdmitted(candidateHost) {
				w.Header().Set("X-Paperboat-Error", "preview_reconnecting")
				w.Header().Set("Retry-After", "2")
				writeErrorPage(w, http.StatusServiceUnavailable, previewReconnectingMessage)
				return
			}
			if p.config.RouteUnavailable != nil && p.config.RouteUnavailable(candidateHost) {
				w.Header().Set("X-Paperboat-Error", "upstream_unavailable")
				writeErrorPage(w, http.StatusBadGateway, upstreamUnavailableMessage)
				return
			}
			writeErrorPage(w, http.StatusNotFound, "")
			return
		}
		matched, lease = acquired, candidateLease
	acquiredRoute:
		defer lease.Close()
		host, expectedKind, ok = matched.Host, string(matched.Rule.Kind), true
		if expectedKind == string(route.TunnelHTTPSWSS) && (matched.Rule.MatchType == route.MatchManagedExact || strings.HasSuffix(host, "."+strings.ToLower(p.config.TunnelBaseDomain))) && !route.ValidManagedTunnelHostname(host, p.config.TunnelBaseDomain) {
			writeErrorPage(w, http.StatusNotFound, "")
			return
		}
		r = r.WithContext(context.WithValue(lease.Context(), routeMatchContextKey{}, matched))
	}
	if !ok || !normalizeRequestTarget(r, host) {
		writeErrorPage(w, http.StatusNotFound, "")
		return
	}
	if headerBytes(r.Header) > p.config.MaxHeaderBytes {
		w.Header().Set("X-Paperboat-Error", "request_headers_too_large")
		writeErrorPage(w, http.StatusRequestHeaderFieldsTooLarge, "")
		return
	}
	if p.config.AddressAllowed != nil && !p.config.AddressAllowed(host, p.clientIP(r)) {
		writeIPRestricted(w, r)
		return
	}
	if r.ContentLength > p.config.MaxBodyBytes {
		w.Header().Set("X-Paperboat-Error", "request_body_too_large")
		writeErrorPage(w, http.StatusRequestEntityTooLarge, "")
		return
	}
	clientIP := p.clientIP(r)
	websocket := isWebSocketUpgrade(r.Header)
	stripHopByHop(r.Header)
	if websocket {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}
	privateStatus, privateAuthorized := http.StatusUnauthorized, false
	var privateRequest connectorprotocol.PrivateAccessRequest
	if p.config.PrivateAccessConnections != nil {
		privateRequest, privateStatus, privateAuthorized = p.config.PrivateAccessConnections.authorizeRequest(r.RemoteAddr, matched)
	}
	r.Header.Del("X-Paperboat-Private-Carrier")
	r.Header.Del("X-Paperboat-Private-Connection")
	browserAuthorized := false
	if (matched.Rule.AccessMode == "private" || matched.Rule.AccessMode == "team") && p.config.BrowserAccess != nil && !privateAuthorized {
		var finish func()
		r, finish, browserAuthorized = p.config.BrowserAccess.authorize(w, r, matched)
		if !browserAuthorized {
			return
		}
		defer finish()
	}
	if matched.Rule.AccessMode == "team" && !browserAuthorized {
		writeErrorPage(w, 401, signInMessage)
		return
	}
	privateRoute := !browserAuthorized && matched.Rule.AccessMode == "private" && (expectedKind == dataCarrierPreviewPrivateRouteKind || expectedKind == string(route.TunnelHTTPSWSS))
	if privateRoute {
		// Private routes are reachable only through stable hostd's authenticated
		// client-initiated carrier stream. Public edge requests never become
		// private access based on browser cookies, Authorization, or custom
		// headers.
		if !privateAuthorized {
			writeErrorPage(w, privateStatus, "")
			return
		}
	}
	if privateAuthorized {
		r = r.WithContext(context.WithValue(r.Context(), privateAccessRequestContextKey{}, privateRequest))
		if ingress, ok := p.config.PrivateAccessConnections.ingress(r.Context(), r.RemoteAddr, matched); ok {
			r = r.WithContext(context.WithValue(r.Context(), privateIngressDecisionKey{}, ingress))
		}
	}
	if rules := matched.Rule.RewriteRules; rules != nil && expectedKind == string(route.TunnelHTTPSWSS) {
		// The route's own rules, after every access check and before the edge's own headers are set, so the edge's
		// headers always win. A redirect never reaches the carrier; a WebSocket handshake keeps its path.
		if !websocket {
			if location, code, redirected := rules.MatchRedirect(r.URL.EscapedPath(), r.URL.RawQuery); redirected {
				w.Header().Set("X-Content-Type-Options", "nosniff")
				http.Redirect(w, r, location, code)
				return
			}
			if rewritten := rules.RewritePath(r.URL.Path); rewritten != r.URL.Path {
				// RawPath keeps what the visitor encoded (%2F stays one segment); net/url ignores it if it no longer
				// matches Path.
				raw := ""
				if r.URL.RawPath != "" {
					raw = rules.RewritePath(r.URL.RawPath)
				}
				r.URL.Path, r.URL.RawPath = rewritten, raw
			}
		}
		tunnelrouterules.ApplyHeaders(r.Header, rules.RequestHeaders)
	}
	stripPrivate(r.Header, expectedKind)
	setVisitorAssertion(r)
	r.Header.Set("X-Forwarded-For", clientIP)
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Port", "443")
	r.Header.Set("X-Real-IP", clientIP)
	info, _ := RequestTelemetryInfo(r.Context())
	requestID, requestIDErr := info.IDs.RequestID, error(nil)
	if !validRequestID(requestID) {
		requestID, requestIDErr = p.trustedRequestID()
	}
	if requestIDErr != nil {
		writeErrorPage(w, http.StatusServiceUnavailable, "")
		return
	}
	r.Header.Set("Forwarded", formatForwarded(clientIP, host))
	r.Header.Set("X-Request-ID", requestID)
	info.IDs.RequestID = requestID
	r = r.WithContext(WithRequestTelemetryInfo(r.Context(), info))
	if matched.Rule.HostOverride != "" {
		r.Host = matched.Rule.HostOverride
	}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &proxyRequestBody{body: r.Body, controller: http.NewResponseController(w), idle: p.config.BodyIdleTimeout}
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.config.MaxBodyBytes)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isAppRouteKind(expectedKind) { // the app's response gets its headers per route (appResponseHeaders)
		r = r.WithContext(context.WithValue(r.Context(), appRouteKey{}, appRoute{gated: matched.Rule.AccessMode != "public", preview: expectedKind != string(route.TunnelHTTPSWSS)}))
	} else {
		setStrictEdgeHeaders(w.Header())
	}
	if p.config.Readiness != nil || matched.Rule.ID != "" {
		kind, state, reason, found := "", "", "", false
		if matched.Rule.ID != "" && matched.Rule.ObservedState != "" {
			kind, state, reason, found = string(matched.Rule.Kind), matched.Rule.ObservedState, matched.Rule.Reason, true
		} else if p.config.Readiness != nil {
			kind, state, reason, found = p.config.Readiness.RouteState(host)
			// A name the server's held ingress snapshot still admits is an app that lost its carrier for a moment (its
			// host restarted or its network blinked), not a name that is gone: wait for it, then a retryable 503.
			admitted := p.config.PreviewAdmitted != nil && p.config.PreviewAdmitted(host)
			if !found && (admitted || p.config.PreviewPending != nil && p.config.PreviewPending(host)) {
				if release, held := p.previewWaits.acquire(host, true); held {
					kind, state, reason, found = p.awaitPreviewRoute(r.Context(), host)
					release()
				}
				if !found {
					if admitted {
						w.Header().Set("X-Paperboat-Error", "preview_reconnecting")
						w.Header().Set("Retry-After", "2")
						writeErrorPage(w, http.StatusServiceUnavailable, previewReconnectingMessage)
						return
					}
					w.Header().Set("X-Paperboat-Error", "preview_starting")
					w.Header().Set("Retry-After", "1")
					writeErrorPage(w, http.StatusServiceUnavailable, previewStartingMessage)
					return
				}
			}
		} else if matched.Rule.ID != "" {
			kind, state, found = string(matched.Rule.Kind), "ready", true
		}
		if !found || kind != expectedKind {
			writeErrorPage(w, http.StatusNotFound, "")
			return
		}
		if kind == "runtime_https_wss" && state != "ready" {
			w.Header().Set("Retry-After", "5")
			writeErrorPage(w, http.StatusServiceUnavailable, "")
			return
		}
		if (kind == "preview_public_https_wss" || kind == dataCarrierPreviewPrivateRouteKind) && state != "ready" {
			status, retry := PreviewHTTPStatus(state, reason)
			if retry {
				w.Header().Set("Retry-After", "5")
			}
			if websocket && retry && closeRetryableWebSocket(w, r) {
				return
			}
			writeErrorPage(w, status, "")
			return
		}
	}
	if expectedKind == "runtime_https_wss" && !helperPublicPath(r.URL.Path) {
		writeErrorPage(w, http.StatusNotFound, "")
		return
	}
	if expectedKind == "runtime_https_wss" && r.URL.Path == "/v1/browser-terminal" && websocket {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || p.config.BrowserTerminal == nil || p.config.BrowserTerminalHub == nil || p.config.RuntimeCarrierTransport == nil {
			writeErrorPage(w, http.StatusServiceUnavailable, "The browser terminal is not available right now. Wait a moment and reload the page.")
			return
		}
		ticket, valid := browserTerminalTicket(r.Header)
		if !valid || len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") == "" {
			writeErrorPage(w, http.StatusForbidden, "This terminal link has expired. Open the terminal again from the Paperboat console.")
			return
		}
		p.serveBrowserTerminal(w, r, host, matched, ticket)
		return
	}
	if expectedKind == "runtime_https_wss" && helperAccessPath(r.URL.Path) {
		claims, ok := p.authorizeHelperAccess(w, r)
		if !ok {
			return
		}
		if matched.Rule.ResourceKind == "runtime" && claims.MachineID != matched.Rule.HostID {
			writeErrorPage(w, http.StatusForbidden, "")
			return
		}
		if !credentialAllowsHelperPath(claims.CredentialClass, r.Method, r.URL.Path) {
			writeErrorPage(w, http.StatusForbidden, "")
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		slog.InfoContext(ctx, "runtime_helper_access", "path", r.URL.Path, "credential_class", claims.CredentialClass, "assignment_present", claims.AssignmentID != "")
		// The edge accounts no helper stream: what a CLI or host carries (exec, ssh, terminal,
		// file transfer, inbox, private access) is reported natively by the host runtime, which
		// also meters it when it rides this edge. Only console browser terminals, which have no
		// native reporter, are accounted here (browser terminal bridge).
		r = r.WithContext(ctx)
		go p.cancelWhenAccessRevoked(ctx, cancel, claims)
	}
	p.next.ServeHTTP(w, r)
}

func lazyPreviewHostname(rawHost, canonicalHost, baseDomain string) bool {
	baseDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(baseDomain), "."))
	if rawHost != canonicalHost || canonicalHost != strings.ToLower(canonicalHost) || baseDomain == "" {
		return false
	}
	label, ok := strings.CutSuffix(canonicalHost, "."+baseDomain)
	if !ok || strings.Contains(label, ".") || !strings.HasPrefix(label, "p") {
		return false
	}
	portText, environment, ok := strings.Cut(label[1:], "-")
	if !ok || len(environment) != 16 || portText == "" || portText[0] == '0' {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	for _, r := range environment {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func normalizeRequestTarget(r *http.Request, canonicalHost string) bool {
	if !r.URL.IsAbs() {
		return true
	}
	absoluteHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.URL.Host), "."))
	if parsed, _, err := net.SplitHostPort(absoluteHost); err == nil {
		absoluteHost = parsed
	}
	if !strings.EqualFold(r.URL.Scheme, "https") || absoluteHost != canonicalHost || r.URL.User != nil {
		return false
	}
	r.URL.Scheme = ""
	r.URL.Host = ""
	return true
}

func helperPublicPath(path string) bool {
	return path == "/healthz" || helperAccessPath(path)
}

func (p *Policy) authorizeHelperAccess(w http.ResponseWriter, r *http.Request) (admission.Claims, bool) {
	if p.config.HelperAccess == nil || p.config.Revocations == nil || p.config.RevocationCheckInterval <= 0 {
		writeErrorPage(w, http.StatusServiceUnavailable, "")
		return admission.Claims{}, false
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) != token || token == "" {
		writeErrorPage(w, http.StatusUnauthorized, "")
		return admission.Claims{}, false
	}
	claims, err := p.config.HelperAccess.VerifyHelperAccess(r.Context(), token)
	if err != nil || claims.Revoked {
		writeErrorPage(w, http.StatusUnauthorized, "")
		return admission.Claims{}, false
	}
	return claims, true
}

func (p *Policy) cancelWhenAccessRevoked(ctx context.Context, cancel context.CancelFunc, claims admission.Claims) {
	ticker := time.NewTicker(p.config.RevocationCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			revoked, err := p.config.Revocations.Revoked(ctx, claims)
			if err != nil || revoked {
				cancel()
				return
			}
		}
	}
}

func helperAccessPath(path string) bool {
	return path == "/v1/runtime" || path == "/v1/browser-terminal" || path == "/v1/preview-launches" || path == "/v1/file-transfers" || strings.HasPrefix(path, "/v1/file-transfers/") ||
		path == "/v1/inbox-files" || strings.HasPrefix(path, "/v1/inbox-files/")
}

// credentialAllowsHelperPath keeps each credential class on its own capability path; the Inbox paths
// also depend on the method (list and read are GETs, delete is a DELETE).
func credentialAllowsHelperPath(class, method, path string) bool {
	switch {
	case path == "/v1/inbox-files":
		return class == "inbox_list" && method == http.MethodGet
	case strings.HasPrefix(path, "/v1/inbox-files/"):
		return class == "inbox_read" && method == http.MethodGet || class == "inbox_delete" && method == http.MethodDelete
	case path == "/v1/runtime":
		return class == "terminal_operation"
	case path == "/v1/browser-terminal":
		return class == "browser_terminal_operation"
	case path == "/v1/preview-launches":
		return class == "preview_launch"
	case path == "/v1/file-transfers" || strings.HasPrefix(path, "/v1/file-transfers/"):
		return class == "file_transfer"
	default:
		return false
	}
}

func closeRetryableWebSocket(w http.ResponseWriter, r *http.Request) bool {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 16 || r.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return false
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return false
	}
	defer connection.Close()
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) // #nosec G401 -- mandated by RFC 6455.
	accept := base64.StdEncoding.EncodeToString(digest[:])
	if _, err := buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\nX-Robots-Tag: noindex, nofollow, noarchive\r\n\r\n"); err != nil {
		return true
	}
	payload := make([]byte, 2+len("Try Again Later"))
	binary.BigEndian.PutUint16(payload, 1013)
	copy(payload[2:], "Try Again Later")
	_ = writeWebSocketClose(buffer, payload)
	return true
}

func writeWebSocketClose(writer *bufio.ReadWriter, payload []byte) error {
	if err := writer.WriteByte(0x88); err != nil {
		return err
	}
	if err := writer.WriteByte(byte(len(payload))); err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	return writer.Flush()
}

func PreviewHTTPStatus(state, reason string) (int, bool) {
	switch state {
	case "registering":
		return http.StatusServiceUnavailable, true
	case "degraded":
		if reason == "target_unhealthy" {
			return http.StatusBadGateway, false
		}
		return http.StatusServiceUnavailable, true
	case "offline":
		return http.StatusServiceUnavailable, true
	case "expired":
		return http.StatusGone, false
	case "removed", "":
		return http.StatusNotFound, false
	case "ready":
		return http.StatusOK, false
	default:
		return http.StatusNotFound, false
	}
}

func (p *Policy) allowedHost(value string) (string, string, bool) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	if p.config.SelfHosted && p.config.Routes != nil {
		if matched, err := p.config.Routes.Match(host, "/"); err == nil && string(matched.Rule.Kind) == "runtime_https_wss" {
			return host, "runtime_https_wss", true
		}
	}
	for _, candidate := range []struct{ domain, kind string }{
		{p.config.PreviewBaseDomain, "preview_public_https_wss"},
		{p.config.RuntimeBaseDomain, "runtime_https_wss"},
	} {
		if candidate.domain == "" {
			continue
		}
		suffix := "." + strings.ToLower(candidate.domain)
		prefix, ok := strings.CutSuffix(host, suffix)
		if ok && prefix != "" && !strings.Contains(prefix, ".") && net.ParseIP(host) == nil {
			return host, candidate.kind, true
		}
	}
	return host, "", false
}

func dispatchHost(value string) (string, bool) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if host == "" || strings.ContainsAny(host, "\r\n\x00 /?") {
		return "", false
	}
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	} else if strings.Contains(host, ":") {
		return "", false
	}
	if host == "" || net.ParseIP(host) != nil {
		return "", false
	}
	return host, true
}

// admitAddress applies the edge-wide limit for the request's client address before any route or
// origin work; over it, the visitor gets the busy answer and nothing else runs.
func (p *Policy) admitAddress(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if p.addresses == nil {
		return func() {}, true
	}
	release, ok := p.addresses.admit(p.clientIP(r), isWebSocketUpgrade(r.Header))
	if !ok {
		if host, valid := dispatchHost(r.Host); valid {
			edgetelemetry.NoteRefusal(host)
		}
		writeRefusal(w, r, http.StatusTooManyRequests, "edge_rate_limited", busyTitle, busyMessage, time.Second)
	}
	return release, ok
}

func (p *Policy) clientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
	remote := net.ParseIP(remoteHost)
	if remote == nil || !p.trusted(remote) {
		return remoteHost
	}
	if client := p.clientStrategy.ClientIP(r.Header, r.RemoteAddr); client != "" {
		return client
	}
	return remote.String()
}

func (p *Policy) trustedRequestID() (string, error) {
	if p.config.RequestID != nil {
		if value := p.config.RequestID(); validRequestID(value) {
			return value, nil
		}
	}
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "request_" + value.String(), nil
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character)) {
			return false
		}
	}
	return true
}

func formatForwarded(clientIP, host string) string {
	forValue := "_"
	if parsed := net.ParseIP(strings.TrimSpace(clientIP)); parsed != nil {
		forValue = parsed.String()
		if parsed.To4() == nil {
			forValue = "\"[" + forValue + "]\""
		}
	}
	if host == "" || strings.ContainsAny(host, "\r\n\x00;,") {
		host = "_"
	}
	return "for=" + forValue + ";proto=https;host=" + host
}

func (p *Policy) trusted(ip net.IP) bool {
	for _, network := range p.config.TrustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func headerBytes(headers http.Header) int64 {
	var size int64
	for name, values := range headers {
		size += int64(len(name))
		for _, value := range values {
			size += int64(len(value))
		}
	}
	return size
}

// stripHopByHop drops the connection-scoped and edge-owned headers. "TE: trailers" is the exception: it is how a
// gRPC client says it reads trailers, and strict servers (grpc-java, Envoy) refuse a request without it, so it
// is kept on the hop to the origin like net/http/httputil.ReverseProxy does.
func stripHopByHop(headers http.Header) {
	trailers := httpguts.HeaderValuesContainsToken(headers["Te"], "trailers")
	defer func() {
		if trailers {
			headers.Set("Te", "trailers")
		}
	}()
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-IP", "X-Request-ID"} {
		headers.Del(name)
	}
}

var helperOperationHeaders = map[string]struct{}{
	"x-paperboat-request-id":   {},
	"x-paperboat-operation-id": {},
	"x-paperboat-deadline-ms":  {},
	"x-paperboat-file-name":    {},
	"x-paperboat-file-mime":    {},
	"x-paperboat-file-size":    {},
	"x-paperboat-file-sha256":  {},
}

func stripPrivate(headers http.Header, routeKind string) {
	stripBrowserRequestCredentials(headers)
	for name := range headers {
		normalized := strings.ToLower(name)
		if !strings.HasPrefix(normalized, "x-paperboat-") {
			continue
		}
		_, helperOperationHeader := helperOperationHeaders[normalized]
		if routeKind != "runtime_https_wss" || !helperOperationHeader {
			headers.Del(name)
		}
	}
}

func isWebSocketUpgrade(headers http.Header) bool {
	if !strings.EqualFold(strings.TrimSpace(headers.Get("Upgrade")), "websocket") {
		return false
	}
	for _, value := range strings.Split(headers.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(value), "upgrade") {
			return true
		}
	}
	return false
}

// routeHoldPoll is how often a held request looks for its route again.
const routeHoldPoll = 50 * time.Millisecond

// holdForRoute retries acquire until it succeeds, the grace passes or the request ends, so a request
// that arrives in the gap after a handover is answered by the route the host attaches a moment later
// (anvit, 2026-11-05: three 404s in 490 ms, 2 s after the replacement started). With no grace it is a
// single call.
func holdForRoute(ctx context.Context, grace time.Duration, acquire func() (*route.StreamLease, route.RouteMatch, error)) (*route.StreamLease, route.RouteMatch, error) {
	lease, matched, err := acquire()
	deadline := time.Now().Add(grace)
	for errors.Is(err, route.ErrNoMatch) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return lease, matched, err
		case <-time.After(routeHoldPoll):
		}
		lease, matched, err = acquire()
	}
	return lease, matched, err
}
