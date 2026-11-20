package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

func publicIngressHandler(routes []config.PublicRoute, infrastructureHost, healthAddress string, next http.Handler, reporter *reporting.Reporter) http.Handler {
	configure := func(target *url.URL, stripPrefix string) *httputil.ReverseProxy {
		proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			// ReverseProxy drops the caller's forwarding headers before Rewrite; keep them and append the peer
			// to X-Forwarded-For, as the Director form did.
			for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if value, ok := pr.In.Header[name]; ok {
					pr.Out.Header[name] = value
				}
			}
			if peer, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				if prior := pr.In.Header["X-Forwarded-For"]; len(prior) > 0 {
					peer = strings.Join(prior, ", ") + ", " + peer
				}
				pr.Out.Header.Set("X-Forwarded-For", peer)
			}
			if stripPrefix != "" {
				pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, stripPrefix)
				if pr.Out.URL.Path == "" {
					pr.Out.URL.Path = "/"
				}
			}
		}}
		proxy.ErrorLog = log.New(io.Discard, "", 0)
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
			reporter.ObserveFailure(request.Context(), "upstream_request", err)
			http.Error(writer, "Paperboat could not forward the request to the service. Wait a moment and try again.", http.StatusBadGateway)
		}
		return proxy
	}
	type entry struct {
		host, prefix string
		proxy        *httputil.ReverseProxy
	}
	entries := make([]entry, 0, len(routes))
	for _, route := range routes {
		target := &url.URL{Scheme: "http", Host: route.Upstream}
		stripPrefix := ""
		if route.StripPrefix {
			stripPrefix = route.PathPrefix
		}
		proxy := configure(target, stripPrefix)
		entries = append(entries, entry{host: route.Host, prefix: route.PathPrefix, proxy: proxy})
	}
	healthURL := &url.URL{Scheme: "http", Host: healthAddress}
	healthProxy := configure(healthURL, "")
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host := request.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		host = strings.ToLower(host)
		if host == infrastructureHost {
			if request.URL.Path == "/healthz" {
				healthProxy.ServeHTTP(writer, request)
				return
			}
			if strings.HasPrefix(request.URL.Path, "/v1/browser-terminal/") || strings.HasPrefix(request.URL.Path, "/v1/runtime/") {
				next.ServeHTTP(writer, request)
				return
			}
			http.NotFound(writer, request)
			return
		}
		var selected *entry
		for index := range entries {
			candidate := &entries[index]
			if candidate.host == host && strings.HasPrefix(request.URL.Path, candidate.prefix) && (selected == nil || len(candidate.prefix) > len(selected.prefix)) {
				selected = candidate
			}
		}
		if selected != nil {
			selected.proxy.ServeHTTP(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
