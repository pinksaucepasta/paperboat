package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type blockedKeyTransport struct{ calls atomic.Int32 }

func (b *blockedKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	b.calls.Add(1)
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestProductionKeysDoNotWaitForControlBeforeListenersStart(t *testing.T) {
	transport := &blockedKeyTransport{}
	control, _ := url.Parse("https://control.example.test")
	cache, err := productionJWKSCache(control, transport, filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("listener construction waited for control-plane keys")
	}
	if _, found, err := cache.Lookup(t.Context(), "unknown"); err != nil || found {
		t.Fatal("unknown signing key was admitted", err)
	}
	service := &jwksRefreshService{cache: cache}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for transport.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("background key refresh did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := service.Shutdown(t.Context()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
