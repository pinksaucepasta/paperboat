package edgehttp

import (
	"html/template"
	"net/http"
)

// Every visitor-facing edge error is this page: the status code is kept, the body says in one plain sentence
// what to do. Prior art: cloudflared answers an unreachable origin with a fixed 502 (~/ref/cloudflared/proxy/proxy.go
// "Unable to reach the origin service"), keeping the code and naming the cause.
var errorPageTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} - Paperboat</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#fff;color:#0f172a;font-family:Figtree,system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:30rem;padding:2rem}
.brand{font-weight:600;letter-spacing:-.025em;font-size:1.5rem;color:#1d52eb;margin:0 0 2rem}
h1{font-size:1.25rem;margin:0 0 .5rem}p{margin:0;line-height:1.5;color:#475569}
</style></head><body><main><p class="brand">paperboat</p><h1>{{.Title}}</h1><p>{{.Message}}</p>{{if .LinkURL}}<p style="margin-top:1rem"><a href="{{.LinkURL}}" style="color:#1d52eb">{{.LinkText}}</a></p>{{end}}</main></body></html>
`))

// deniedMessage is the 403 sentence for a signed-in account without access. Prior art: cloudflared answers a failed
// Access JWT check with 403 on the app address (~/ref/cloudflared/ingress/middleware/jwtvalidator.go); Teleport app
// access expects 403 for a denied user (~/ref/teleport/lib/web/app/handler_test.go).
const deniedMessage = "This account does not have access to this app."

const (
	signInMessage     = "Sign in to Paperboat in your browser, then open this address again."
	retryMessage      = "Paperboat could not finish this request. Wait a moment and reload the page."
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

// writeErrorPage answers with the branded page. An empty message uses the status's default sentence.
func writeErrorPage(w http.ResponseWriter, status int, message string) {
	writeErrorPageLink(w, status, message, "", "")
}

// writeErrorPageLink adds one link under the sentence (an empty linkURL adds none).
func writeErrorPageLink(w http.ResponseWriter, status int, message, linkText, linkURL string) {
	if message == "" {
		message = defaultErrorMessages[status]
	}
	if message == "" {
		message = retryMessage
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = errorPageTemplate.Execute(w, struct{ Title, Message, LinkText, LinkURL string }{http.StatusText(status), message, linkText, linkURL})
}
