package edgehttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	edgetelemetry "github.com/pinksaucepasta/paperboat/tunnel/internal/telemetry"
)

// Every visitor-facing edge error is this page: the status code is kept, the body says in plain words what
// happened and what to do, and a reference id ties the page to the edge log line for that failure. The look is the
// console's brand screen (console/components/brand-screen.tsx: brand-bar gradient, boat + wordmark top left, the
// Heading type scale); dark mode follows the console's dark surface. Prior art: cloudflared answers an unreachable
// origin with a fixed 502 (~/ref/cloudflared/proxy/proxy.go "Unable to reach the origin service"), keeping the code
// and naming the cause; Cloudflare's error pages show a Ray ID and the time for support the same way.
//
// The boat is console/app/icon.svg's path (console/components/logo.tsx); the wordmark is "paperboat" in Figtree 500,
// tracking -0.025em, converted to outlines so the page needs no font file. The page asks for no scripts, fonts or
// images: Try again is a link to the same address.
const boatPath = "M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"

const wordmarkPath = "M71.4 200V-500H160.1L164.7 -418.7Q191.9 -462.7 233.9 -487.4Q275.9 -512 329.8 -512Q397.2 -512 448.1 -479.2Q498.9 -446.4 527.3 -387.6Q555.8 -328.7 555.8 -250Q555.8 -172.3 527.3 -112.9Q498.9 -53.6 448.1 -20.8Q397.2 12 329.8 12Q276.2 12 234.6 -12.3Q193.1 -36.7 165.8 -79.8V200ZM307.4 -74.1Q351.6 -74.1 385.7 -96.6Q419.7 -119.1 439.4 -158.8Q459.1 -198.5 459.1 -250Q459.1 -301.5 439.9 -341.2Q420.8 -380.9 386.7 -403.2Q352.7 -425.6 308.4 -425.6Q266.9 -425.6 234.5 -403.2Q202.1 -380.9 184 -341.2Q165.8 -301.5 165.8 -250Q165.8 -198.5 183.9 -158.7Q202.1 -118.9 234 -96.5Q266 -74.1 307.4 -74.1ZM794.8 12Q705.8 12 656.3 -28.3Q606.8 -68.5 606.8 -141Q606.8 -217.9 658.2 -258.6Q709.7 -299.4 802 -299.4H935.6Q930.2 -363.1 902.4 -395.7Q874.7 -428.3 819.6 -428.3Q780.8 -428.3 752.5 -412.2Q724.1 -396.2 705.1 -362.5L621.4 -394.1Q639.1 -428.2 665.8 -454.8Q692.6 -481.4 730.6 -496.7Q768.6 -512 819.6 -512Q890.9 -512 938 -484Q985.1 -456.1 1008.3 -402.8Q1031.5 -349.5 1031 -272.3L1029.6 0H942.1L938.5 -64.3Q917.4 -27.7 881.1 -7.8Q844.8 12 794.8 12ZM803.2 -69.3Q843.6 -69.3 874 -86.6Q904.4 -104 921 -135.3Q937.6 -166.6 937.6 -208.3V-223.8H828.1Q759.5 -223.8 731.1 -202.6Q702.7 -181.4 702.7 -142.9Q702.7 -108.4 729.2 -88.8Q755.7 -69.3 803.2 -69.3ZM1131.4 200V-500H1220.1L1224.7 -418.7Q1251.9 -462.7 1293.9 -487.4Q1335.9 -512 1389.8 -512Q1457.2 -512 1508.1 -479.2Q1558.9 -446.4 1587.3 -387.6Q1615.8 -328.7 1615.8 -250Q1615.8 -172.3 1587.3 -112.9Q1558.9 -53.6 1508.1 -20.8Q1457.2 12 1389.8 12Q1336.2 12 1294.6 -12.3Q1253.1 -36.7 1225.8 -79.8V200ZM1367.4 -74.1Q1411.6 -74.1 1445.7 -96.6Q1479.7 -119.1 1499.4 -158.8Q1519.1 -198.5 1519.1 -250Q1519.1 -301.5 1499.9 -341.2Q1480.8 -380.9 1446.7 -403.2Q1412.7 -425.6 1368.4 -425.6Q1326.9 -425.6 1294.5 -403.2Q1262.1 -380.9 1244 -341.2Q1225.8 -301.5 1225.8 -250Q1225.8 -198.5 1243.9 -158.7Q1262.1 -118.9 1294 -96.5Q1326 -74.1 1367.4 -74.1ZM1916.2 12Q1843.2 12 1787 -21.5Q1730.8 -55 1698.8 -114Q1666.8 -173 1666.8 -250Q1666.8 -327 1699 -386Q1731.2 -445 1787.6 -478.5Q1844 -512 1916.9 -512Q1985.5 -512 2037.9 -477.5Q2090.3 -443 2119.8 -379.6Q2149.4 -316.3 2149.4 -229.6H1763.6Q1768.6 -156.1 1813.1 -114.6Q1857.6 -73.1 1922.9 -73.1Q1972.7 -73.1 2006.3 -96.3Q2039.9 -119.6 2058.6 -156.5L2141.1 -118.3Q2121.1 -78.9 2089.2 -49.5Q2057.3 -20 2013.9 -4Q1970.5 12 1916.2 12ZM1770.1 -305.2H2047.2Q2044.2 -345 2025.6 -372.7Q2007 -400.3 1978.3 -415Q1949.5 -429.8 1915 -429.8Q1881.5 -429.8 1849.4 -415.3Q1817.3 -400.8 1795.7 -372.9Q1774.1 -345 1770.1 -305.2ZM2218.4 0V-500H2312.8V-410.3Q2342.7 -457.3 2390.5 -479.6Q2438.4 -502 2489.8 -502V-412.3Q2444.9 -412.3 2404.4 -398.8Q2363.9 -385.3 2338.4 -354.4Q2312.8 -323.5 2312.8 -271.7V0ZM2800.8 12Q2746.9 12 2705.1 -12.6Q2663.2 -37.3 2635.9 -81.3L2632.1 0H2542.4V-700H2636.8V-420.2Q2664.1 -463.6 2705.6 -487.8Q2747.2 -512 2800.8 -512Q2868.2 -512 2919.1 -479.2Q2969.9 -446.4 2998.3 -387.6Q3026.8 -328.7 3026.8 -250Q3026.8 -172.3 2998.3 -112.9Q2969.9 -53.6 2919.1 -20.8Q2868.2 12 2800.8 12ZM2778.4 -74.1Q2822.6 -74.1 2856.7 -96.6Q2890.7 -119.1 2910.4 -158.8Q2930.1 -198.5 2930.1 -250Q2930.1 -301.5 2910.9 -341.2Q2891.8 -380.9 2857.7 -403.2Q2823.7 -425.6 2779.4 -425.6Q2737.9 -425.6 2705.5 -403.2Q2673.1 -380.9 2655 -341.2Q2636.8 -301.5 2636.8 -250Q2636.8 -198.5 2654.9 -158.7Q2673.1 -118.9 2705 -96.5Q2737 -74.1 2778.4 -74.1ZM3331.6 12Q3257.3 12 3200.1 -21.5Q3142.8 -55 3110.3 -114Q3077.8 -173 3077.8 -250Q3077.8 -327 3109.8 -386Q3141.8 -445 3199 -478.5Q3256.3 -512 3329.5 -512Q3403.8 -512 3461.1 -478.5Q3518.3 -445 3550.3 -386Q3582.3 -327 3582.3 -250Q3582.3 -173 3550.3 -114Q3518.4 -55 3461.6 -21.5Q3404.9 12 3331.6 12ZM3331.6 -74.8Q3377.3 -74.8 3412 -97.2Q3446.7 -119.7 3466.6 -159Q3486.5 -198.4 3486.5 -250Q3486.5 -301.6 3466.6 -341Q3446.6 -380.3 3411 -402.8Q3375.4 -425.2 3329.7 -425.2Q3283.9 -425.2 3248.7 -402.8Q3213.5 -380.3 3193.6 -341Q3173.7 -301.7 3173.7 -250.1Q3173.7 -198.5 3193.9 -159.1Q3214.2 -119.8 3249.9 -97.3Q3285.7 -74.8 3331.6 -74.8ZM3820.8 12Q3731.8 12 3682.3 -28.3Q3632.8 -68.5 3632.8 -141Q3632.8 -217.9 3684.2 -258.6Q3735.7 -299.4 3828 -299.4H3961.6Q3956.2 -363.1 3928.4 -395.7Q3900.7 -428.3 3845.6 -428.3Q3806.8 -428.3 3778.5 -412.2Q3750.1 -396.2 3731.1 -362.5L3647.4 -394.1Q3665.1 -428.2 3691.8 -454.8Q3718.6 -481.4 3756.6 -496.7Q3794.6 -512 3845.6 -512Q3916.9 -512 3964 -484Q4011.1 -456.1 4034.3 -402.8Q4057.5 -349.5 4057 -272.3L4055.6 0H3968.1L3964.5 -64.3Q3943.4 -27.7 3907.1 -7.8Q3870.8 12 3820.8 12ZM3829.2 -69.3Q3869.6 -69.3 3900 -86.6Q3930.4 -104 3947 -135.3Q3963.6 -166.6 3963.6 -208.3V-223.8H3854.1Q3785.5 -223.8 3757.1 -202.6Q3728.7 -181.4 3728.7 -142.9Q3728.7 -108.4 3755.2 -88.8Q3781.7 -69.3 3829.2 -69.3ZM4345.4 12Q4269.6 12 4228.8 -26.7Q4188.1 -65.3 4188.1 -136.6V-417.8H4103.1V-500H4188.1V-656H4282.5V-500H4425.5V-417.8H4282.5V-150.6Q4282.5 -114.1 4301.2 -94.6Q4319.9 -75.1 4354.6 -75.1Q4365.5 -75.1 4377.1 -79Q4388.8 -83 4406.5 -95.1L4441.8 -20.7Q4415.9 -3.6 4392.5 4.2Q4369 12 4345.4 12Z"

var errorPageTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}} - Paperboat</title>
<style>
*{box-sizing:border-box}
body{margin:0;min-height:100svh;background:linear-gradient(to bottom,#4275f8 0%,#1d52eb 55%,#0f3dd8 100%);color:#fff;font-family:Figtree,system-ui,-apple-system,"Segoe UI",sans-serif;-webkit-font-smoothing:antialiased}
main{display:flex;min-height:100svh;flex-direction:column;align-items:center;justify-content:center;padding:96px 24px}
.brand{position:absolute;top:24px;left:24px;display:flex;align-items:center;gap:12px}
.brand svg{height:18px;display:block;fill:currentColor}
.content{display:flex;width:100%;max-width:36rem;flex-direction:column;gap:24px}
.text{display:flex;flex-direction:column;gap:12px}
h1{margin:0;font-size:clamp(1.75rem,4vw,2.5rem);line-height:1.1;font-weight:600;letter-spacing:-.03em;text-wrap:balance}
p{margin:0}
.message{font-size:1rem;line-height:1.5;color:rgba(255,255,255,.75)}
.action{align-self:flex-start;display:inline-flex;height:40px;align-items:center;padding:0 16px;border-radius:8px;background:#fff;color:#0f3dd8;font-size:.875rem;font-weight:500;text-decoration:none}
.action:hover{background:rgba(255,255,255,.9)}
.action:focus-visible{outline:2px solid rgba(255,255,255,.5);outline-offset:2px}
.meta{font-size:.8125rem;line-height:1.4;color:rgba(255,255,255,.65);overflow-wrap:anywhere}
.ref{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.95em;user-select:all}
@media (prefers-color-scheme:dark){
body{background:#121212;color:#f5f5f5}
.message{color:#9b9b9b}
.action{background:#1d52eb;color:#fff}
.action:hover{background:#4275f8}
.action:focus-visible{outline-color:rgba(255,255,255,.5)}
.meta{color:#9b9b9b}
}
</style></head><body><main>
<div class="brand" role="img" aria-label="Paperboat">
<svg viewBox="0 0 600 360" aria-hidden="true"><defs><linearGradient id="boat" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fff"/><stop offset="1" stop-color="#eaf0ff"/></linearGradient></defs><path fill="url(#boat)" fill-rule="evenodd" d="` + boatPath + `"/></svg>
<svg viewBox="0 -760 4445 1000" aria-hidden="true"><path d="` + wordmarkPath + `"/></svg>
</div>
<div class="content">
<div class="text"><h1>{{.Title}}</h1><p class="message">{{.Message}}</p></div>
{{if .ActionText}}<a class="action" href="{{.ActionURL}}">{{.ActionText}}</a>{{end}}
<p class="meta">Error {{.Status}} · Reference <code class="ref">{{.Reference}}</code> · {{.Time}}</p>
</div>
</main></body></html>
`))

// deniedMessage is the 403 sentence for a signed-in account without access. Prior art: cloudflared answers a failed
// Access JWT check with 403 on the app address (~/ref/cloudflared/ingress/middleware/jwtvalidator.go); Teleport app
// access expects 403 for a denied user (~/ref/teleport/lib/web/app/handler_test.go).
const deniedMessage = "This account does not have access to this app."

const (
	signInMessage     = "Sign in to Paperboat in your browser, then open this address again."
	retryMessage      = "Wait a moment and try again. If it keeps failing, send the reference below to Paperboat support."
	notFoundMessage   = "This address does not exist, or your account cannot open it. Check the link or ask its owner to share it with you."
	badRequestMessage = "Paperboat could not understand this request. Open the address again from its original link."
)

var defaultErrorMessages = map[int]string{
	http.StatusBadRequest:            badRequestMessage,
	http.StatusUnauthorized:          signInMessage,
	http.StatusForbidden:             "Your account cannot open this address. Ask its owner to share it with you.",
	http.StatusNotFound:              notFoundMessage,
	http.StatusConflict:              retryMessage,
	http.StatusRequestEntityTooLarge: "This request is too large. Send less data and try again.",
	http.StatusTooManyRequests:       edgeRateLimitedMessage,
	http.StatusBadGateway:            upstreamUnavailableMessage,
	http.StatusServiceUnavailable:    retryMessage,
	http.StatusGatewayTimeout:        retryMessage,
}

// errorTitles say what happened in plain words; the HTTP reason phrase never reaches a visitor.
const defaultErrorTitle = "Paperboat couldn't finish this request"

var errorTitles = map[int]string{
	http.StatusBadRequest:            "This link doesn't work",
	http.StatusUnauthorized:          "Sign in to continue",
	http.StatusForbidden:             "You don't have access to this app",
	http.StatusNotFound:              "Page not found",
	http.StatusRequestEntityTooLarge: "Request too large",
	http.StatusTooManyRequests:       "Too many requests",
	http.StatusBadGateway:            "This app isn't responding",
}

// referenceHeader carries the same id the page shows and the edge logs, so support finds the failure from either.
const referenceHeader = "X-Paperboat-Reference"

// readRandom is crypto/rand.Read; a variable so a test can make it fail.
var readRandom = rand.Read

var referenceCounter atomic.Uint64

// newReference is the support reference every Paperboat surface shows: support_ and a version 4 UUID, the format the
// server and the CLI mint (server/internal/supportreport NewReference), so one search finds it anywhere. If the system
// random source fails, the bytes are the clock mixed with a counter, never the same twice.
func newReference() string {
	var b [16]byte
	if _, err := readRandom(b[:]); err != nil {
		for half := 0; half < 2; half++ {
			v := uint64(time.Now().UnixNano()) ^ referenceCounter.Add(1)*0x9e3779b97f4a7c15
			for i := 0; i < 8; i++ {
				b[half*8+i] = byte(v >> (8 * i))
			}
		}
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return "support_" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// sampledLog lets through at most limit lines per second and tells the first line of the next second how many it
// dropped in between.
type sampledLog struct {
	mu         sync.Mutex
	window     time.Time
	logged     int
	suppressed int
	limit      int
}

// fourxxLogPerSecond is the most edge_http_4xx lines logged per second.
const fourxxLogPerSecond = 20

var fourxxSample = &sampledLog{limit: fourxxLogPerSecond}

// allow reports whether this line may be logged and, if so, how many were dropped since the last one that was.
func (s *sampledLog) allow(now time.Time) (ok bool, suppressed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.window) >= time.Second {
		s.window, s.logged = now, 0
	}
	if s.logged >= s.limit {
		s.suppressed++
		return false, 0
	}
	s.logged++
	suppressed, s.suppressed = s.suppressed, 0
	return true, suppressed
}

// writeErrorPage answers with the branded page. An empty message uses the status's default sentence.
func writeErrorPage(w http.ResponseWriter, status int, message string) {
	writeErrorPageLink(w, status, message, "", "")
}

// writeErrorPageLink makes one link the page's action (an empty linkURL keeps the status's default action: Try again
// for failures a retry can clear, Sign in for 401).
func writeErrorPageLink(w http.ResponseWriter, status int, message, linkText, linkURL string) {
	if message == "" {
		message = defaultErrorMessages[status]
	}
	if message == "" {
		message = retryMessage
	}
	title := errorTitles[status]
	if title == "" {
		title = defaultErrorTitle
	}
	if linkURL == "" {
		switch status {
		case http.StatusUnauthorized:
			linkText, linkURL = "Sign in", "/"
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusConflict, http.StatusTooManyRequests:
			linkText = "Try again" // an empty href reloads the address
		}
	}
	ref := newReference()
	switch {
	case status >= http.StatusInternalServerError:
		// Every 5xx the edge itself answers is a user-visible failure; the deploy gate counts these.
		edgetelemetry.Log(context.Background(), edgetelemetry.SeverityWarn, "edge_http_5xx", edgetelemetry.OutcomeFailed, "status", status, "reference", ref)
		edgetelemetry.NoteErrorPage(ref, status)
	case status >= http.StatusBadRequest:
		// A 4xx flood (a scanner, a dead link in a page) must not become a log flood: sampled, 5xx never are. The same
		// sample decides which 4xx references go to the server's support_reports.
		if ok, suppressed := fourxxSample.allow(time.Now()); ok {
			edgetelemetry.Log(context.Background(), edgetelemetry.SeverityInfo, "edge_http_4xx", edgetelemetry.OutcomeRejected, "status", status, "reference", ref, "suppressed", suppressed)
			edgetelemetry.NoteErrorPage(ref, status)
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(referenceHeader, ref)
	// The page's only active content is its own inline style; this also replaces the stricter header some callers set.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; form-action 'none'")
	w.WriteHeader(status)
	_ = errorPageTemplate.Execute(w, struct {
		Title, Message, ActionText, ActionURL, Reference, Time string
		Status                                                 int
	}{title, message, linkText, linkURL, ref, time.Now().UTC().Format("2006-01-02 15:04:05 UTC"), status})
}
