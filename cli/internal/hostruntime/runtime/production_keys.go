package runtime

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"net/http"
	"net/url"
	"time"
)

func productionJWKSCache(control *url.URL, transport http.RoundTripper, path string) (*auth.JWKSCache, error) {
	fetcher, err := auth.NewHTTPJWKSFetcher(control.ResolveReference(&url.URL{Path: "/.well-known/jwks.json"}).String(), []string{control.Hostname()}, transport)
	if err != nil {
		return nil, err
	}
	cache, err := auth.NewJWKSCache(auth.JWKSConfig{Fetcher: fetcher, Clock: productionClock{}, TTL: 5 * time.Minute, RetainMissing: auth.DefaultRetainMissing, PersistencePath: path})
	if err != nil {
		return nil, err
	}
	// Loading persisted keys is local. jwksRefreshService renews asynchronously;
	// the verifier refreshes a missing key before deciding authorization.
	return cache, nil
}
