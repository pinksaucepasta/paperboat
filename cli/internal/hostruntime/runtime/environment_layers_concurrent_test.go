//go:build darwin || linux

package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
)

// 32 execs started together must cost about one control-plane round trip, not 32 in a row (a hosted Mac at
// 0.65 s RTT took 21 s for the burst while the layer request held layerMu).
func TestConcurrentLayerLaunchesShareOneRoundTrip(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	registration := layerRegistration(t)
	registration.AccountID = "account_1"
	root := filepath.Join(t.TempDir(), "runtime")
	source, err := productionEnvironmentKeySourceForState(root, registration)
	if err != nil {
		t.Fatal(err)
	}
	material, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.(interface{ EstablishLayerGenesis() error }).EstablishLayerGenesis(); err != nil {
		t.Fatal(err)
	}
	const rtt = 300 * time.Millisecond
	credentials := &layerTestCredentials{token: "test-token", proof: []byte("machine-proof")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environment/hosts/machine_1/key" {
			writeLayerRecipientRegistrationResponse(t, w, r, credentials, registration, material.Generation, public[:])
			return
		}
		time.Sleep(rtt)
		bundle := api.VaultLayerContext{WorkspaceID: "personal", ActorAccountID: "account_1", MachineID: registration.MachineID, Layers: []api.VaultLayerDelivery{}, Recipient: api.VaultLayerRecipient{RecipientAccount: registration.AccountID, MachineID: registration.MachineID, InstallationGeneration: uint64(registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: base64.RawURLEncoding.EncodeToString(public[:])}}
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"data": bundle})
	}))
	defer server.Close()
	service := newLayerEnvironmentService(root, layerServiceURL(t, server.URL), server.Client().Transport, registration, credentials, credentials)
	ctx := envinject.WithLaunchContext(context.Background(), "personal", "account_1")
	if _, err := service.EnvironmentForLaunch(ctx); err != nil { // registers the recipient once
		t.Fatal(err)
	}
	began := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.EnvironmentForLaunch(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(began); took > 5*rtt {
		t.Fatalf("32 concurrent launches took %v at %v per request: the round trips are serialized", took, rtt)
	}
}
