package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestControlPlaneReachable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer up.Close()
	if !ControlPlaneReachable(context.Background(), up.URL, time.Second) {
		t.Error("a server that answers (even 503) is reachable")
	}
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer stalled.Close()
	defer close(release)
	started := time.Now()
	if ControlPlaneReachable(context.Background(), stalled.URL, 100*time.Millisecond) || time.Since(started) > 2*time.Second {
		t.Errorf("a server that never answers is unreachable within the bound (took %v)", time.Since(started))
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if ControlPlaneReachable(context.Background(), url, time.Second) {
		t.Error("a closed port is unreachable")
	}
}
