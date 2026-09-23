package main

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
)

func publicIngressHandler(routes []config.PublicRoute, infrastructureHost, healthAddress string, next http.Handler) http.Handler {
	type entry struct {
		host, prefix string
		proxy        *httputil.ReverseProxy
	}
	entries := make([]entry, 0, len(routes))
	for _, route := range routes {
		target := &url.URL{Scheme: "http", Host: route.Upstream}
		proxy := httputil.NewSingleHostReverseProxy(target)
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
