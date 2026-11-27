package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerAPIPreflightRefusesTheConsole(t *testing.T) {
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html></html>"))
	}))
	defer console.Close()
	if err := checkServerIsAPI(context.Background(), console.Client(), console.URL); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("console URL accepted: %v", err)
	}
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	if err := checkServerIsAPI(context.Background(), notFound.Client(), notFound.URL); err == nil {
		t.Fatal("a server without client configuration accepted")
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/client-configuration" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"1","machines_url":"https://app.example.test/machines"}`))
	}))
	defer api.Close()
	if err := checkServerIsAPI(context.Background(), api.Client(), api.URL); err != nil {
		t.Fatal(err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	if err := checkServerIsAPI(context.Background(), down.Client(), down.URL); err != nil {
		t.Fatalf("a temporary server failure refused pairing: %v", err)
	}
}

func TestPairWithoutServerURLFailsBeforeBootstrap(t *testing.T) {
	for _, server := range []string{"", "http://console.example.test"} {
		if _, err := pairPreflightServerURL(server); err == nil || !strings.Contains(err.Error(), "--server") {
			t.Fatalf("server %q: %v", server, err)
		}
	}
}
