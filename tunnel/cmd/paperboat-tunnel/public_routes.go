package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
)

func publicIngressHandler(routes []config.PublicRoute, infrastructureHost, healthAddress string, next http.Handler, reporter *reporting.Reporter) http.Handler {
	configure := func(proxy *httputil.ReverseProxy) {
		proxy.ErrorLog = log.New(io.Discard, "", 0)
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, err error) {
			reporter.ObserveFailure(request.Context(), "upstream_request", err)
			http.Error(writer, "upstream unavailable", http.StatusBadGateway)
		}
	}
	type entry struct {
		host, prefix string
		proxy        *httputil.ReverseProxy
	}
	entries := make([]entry, 0, len(routes))
	for _, route := range routes {
		target := &url.URL{Scheme: "http", Host: route.Upstream}
		proxy := httputil.NewSingleHostReverseProxy(target)
		configure(proxy)
		if route.StripPrefix {
			base := proxy.Director
			prefix := route.PathPrefix
			proxy.Director = func(request *http.Request) {
				base(request)
				request.URL.Path = strings.TrimPrefix(request.URL.Path, prefix)
				if request.URL.Path == "" {
					request.URL.Path = "/"
				}
			}
		}
		entries = append(entries, entry{host: route.Host, prefix: route.PathPrefix, proxy: proxy})
	}
	healthURL := &url.URL{Scheme: "http", Host: healthAddress}
	healthProxy := httputil.NewSingleHostReverseProxy(healthURL)
	configure(healthProxy)
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
			if strings.HasPrefix(request.URL.Path, "/v1/browser-terminal/") || strings.HasPrefix(request.URL.Path, "/v1/runtime/") || strings.HasPrefix(request.URL.Path, "/v1/browser-config-compare/") {
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
