package nodelifecycle

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// bodyServingPin is the serving_spki_sha256 field of the last request, "" when the request omits it.
func bodyServingPin(t *testing.T, api *meshAPI) string {
	t.Helper()
	raw, ok := api.bodyFields(t)["serving_spki_sha256"]
	if !ok {
		return ""
	}
	var pin string
	if err := json.Unmarshal(raw, &pin); err != nil {
		t.Fatal(err)
	}
	return pin
}

func TestObserveAndStartBodiesCarryServingPin(t *testing.T) {
	api := newMeshAPI(t)
	pin := "sha256:" + strings.Repeat("ab", 32)
	c, err := New(Config{URL: api.srv.URL, HTTP: api.srv.Client(), Credential: strings.Repeat("test", 16), NodeID: "relay", ExpectedGeneration: 1,
		StatePath: filepath.Join(t.TempDir(), "state"), ServingSPKISHA256: pin})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	lease, err := c.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := bodyServingPin(t, api); got != pin {
		t.Fatalf("start body serving pin %q, want %q", got, pin)
	}
	if err := c.Observe(t.Context(), newMeshServer(t, api, lease, false), false); err != nil {
		t.Fatal(err)
	}
	if got := bodyServingPin(t, api); got != pin {
		t.Fatalf("observe body serving pin %q, want %q", got, pin)
	}

	bare, bareLease := newMeshClient(t, api)
	if got := bodyServingPin(t, api); got != "" {
		t.Fatalf("start body of a relay without a serving pin carries %q", got)
	}
	if err := bare.Observe(t.Context(), newMeshServer(t, api, bareLease, false), false); err != nil {
		t.Fatal(err)
	}
	if got := bodyServingPin(t, api); got != "" {
		t.Fatalf("observe body of a relay without a serving pin carries %q", got)
	}
}
