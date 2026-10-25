package control

import (
	"io"
	"net"
	"net/http"
	"strings"
)

const acmeHTTP01Prefix = "/.well-known/acme-challenge/"

// HTTP01Handler answers ACME http-01 on the plain HTTP listener for the
// hostnames the current ingress snapshot holds a response for. Every other
// request goes to next (the HTTPS redirect). The body is the key authorization
// the server issued, so the edge never invents one.
func (a *IngressAuthority) HTTP01Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.URL.Path, acmeHTTP01Prefix)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		keyAuthorization, found := a.HTTP01KeyAuthorization(strings.ToLower(host), token)
		if r.Method != http.MethodGet || !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, keyAuthorization)
	})
}
