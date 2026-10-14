package tunnelcert

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDNS01ZonesUseMatchingZoneForCreateAndCleanup(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		fmt.Fprint(w, `{"success":true,"result":{"id":"record_test"}}`)
	}))
	defer server.Close()
	providers := map[string]DNS01Provider{}
	for suffix, zone := range map[string]string{"public.example.test": "zone_public", "runtime.example.test": "zone_runtime"} {
		provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{BaseURL: server.URL, ZoneID: zone, TokenReference: "secret/test", TokenSource: testSecretSource{value: []byte("fixture-only")}, Resolver: &testTXTResolver{values: [][]string{{"fixture-value"}}}})
		if err != nil {
			t.Fatal(err)
		}
		providers[suffix] = provider
	}
	zones, err := NewDNS01Zones(providers)
	if err != nil {
		t.Fatal(err)
	}
	for suffix, zone := range map[string]string{"public.example.test": "zone_public", "runtime.example.test": "zone_runtime"} {
		record, err := zones.Present(t.Context(), DNS01Record{Domain: "*." + suffix, Name: "_acme-challenge." + suffix, Value: "fixture-value", TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := zones.Wait(t.Context(), record); err != nil {
			t.Fatal(err)
		}
		if err := zones.Cleanup(t.Context(), record); err != nil {
			t.Fatal(err)
		}
		got := paths[len(paths)-2:]
		want := []string{"POST /zones/" + zone + "/dns_records", "DELETE /zones/" + zone + "/dns_records/record_test"}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("zone lifecycle used wrong authority: %v", got)
		}
	}
	before := len(paths)
	record := DNS01Record{Domain: "*.unconfigured.example.test", Name: "_acme-challenge.unconfigured.example.test", Value: "fixture-value", ProviderID: "record_test", TTL: time.Minute}
	if _, err := zones.Present(context.Background(), record); err == nil {
		t.Fatal("unconfigured zone admitted")
	}
	if err := zones.Wait(context.Background(), record); err == nil {
		t.Fatal("unconfigured zone waited")
	}
	if err := zones.Cleanup(context.Background(), record); err == nil {
		t.Fatal("unconfigured zone deleted")
	}
	if len(paths) != before {
		t.Fatal("unconfigured name reached provider")
	}
}
