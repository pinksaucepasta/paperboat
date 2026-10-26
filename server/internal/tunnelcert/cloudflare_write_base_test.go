package tunnelcert

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A staging server that shares a Cloudflare token with a production zone sets WriteBase: it may write
// and delete TXT records only at or under that name, and never an address record. A refused write
// sends nothing to Cloudflare.
func TestCloudflareWriteBaseConfinesEveryWrite(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"record_123"}}`))
	}))
	defer server.Close()
	provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{BaseURL: server.URL + "/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare", TokenSource: testSecretSource{value: []byte("token")}, AddressAdmission: newCloudflareAddressAdmission(), WriteBase: "stg.example.test."})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := provider.Present(ctx, DNS01Record{Domain: "tunnels.stg.example.test", Name: "_acme-challenge.tunnels.stg.example.test", Value: "value", TTL: time.Minute}); err != nil || requests.Load() != 1 {
		t.Fatalf("write under the base = %v after %d requests", err, requests.Load())
	}
	for _, name := range []string{"_acme-challenge.example.test", "_acme-challenge.prod.example.test", "_acme-challenge.xstg.example.test"} {
		if _, err := provider.Present(ctx, DNS01Record{Domain: "example.test", Name: name, Value: "value", TTL: time.Minute}); !errors.Is(err, ErrDNSWriteOutsideBase) {
			t.Fatalf("write to %s = %v, want ErrDNSWriteOutsideBase", name, err)
		}
		if err := provider.Cleanup(ctx, DNS01Record{Name: name, ProviderID: "record_123"}); !errors.Is(err, ErrDNSWriteOutsideBase) {
			t.Fatalf("delete of %s = %v, want ErrDNSWriteOutsideBase", name, err)
		}
	}
	if _, err := provider.CreateAddressRecord(ctx, AddressRecord{Hostname: "edge.stg.example.test", Address: "8.8.8.8", Owner: "route_1", Generation: 1}, false); !errors.Is(err, ErrDNSWriteOutsideBase) {
		t.Fatalf("address record under a write base = %v, want refused", err)
	}
	if err := provider.DeleteAddressRecord(ctx, AddressRecord{Hostname: "edge.stg.example.test", Address: "8.8.8.8", Owner: "route_1", Generation: 1, ProviderID: "record_123"}); !errors.Is(err, ErrDNSWriteOutsideBase) {
		t.Fatalf("address delete under a write base = %v, want refused", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("refused writes reached Cloudflare: %d requests", requests.Load())
	}
}
