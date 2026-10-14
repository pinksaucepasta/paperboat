//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
)

// TestEnvironmentLiveExistingCustody exercises production transport and OS key
// custody without starting a daemon or changing installed binaries. Observation
// delivery is deliberately deferred so this isolated consumer cannot advance the
// installed owner's acknowledgement sequence. Only the encrypted authenticated
// floor is copied; credentials and recipient keys remain in existing custody.
func TestEnvironmentLiveExistingCustody(t *testing.T) {
	root := os.Getenv("PAPERBOAT_ENV_LIVE_STATE_ROOT")
	if root == "" {
		t.Skip("connected ENV qualification is explicitly gated")
	}
	workspace, actor := os.Getenv("PAPERBOAT_ENV_LIVE_WORKSPACE"), os.Getenv("PAPERBOAT_ENV_LIVE_ACTOR")
	if !filepath.IsAbs(root) || workspace == "" || actor == "" {
		t.Fatal("live qualification requires exact coordinates")
	}
	var expected map[string]string
	if json.Unmarshal([]byte(os.Getenv("PAPERBOAT_ENV_LIVE_EXPECTED_SHA256")), &expected) != nil || expected == nil || len(expected) > 128 {
		t.Fatal("expected name/hash fixture is required")
	}
	for name, hash := range expected {
		decoded, err := hex.DecodeString(hash)
		if name == "" || err != nil || len(decoded) != sha256.Size {
			t.Fatal("invalid expected hash fixture")
		}
	}
	readExisting := func(path string, max int64) []byte {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > max {
			t.Fatal("required existing custody or floor is unavailable")
		}
		raw, err := os.ReadFile(path)
		if err != nil || int64(len(raw)) > max {
			t.Fatal("existing custody or floor could not be read")
		}
		return raw
	}
	for _, name := range []string{"machine-identity.json", "machine-registration.json", "runtime-identity.json"} {
		raw := readExisting(filepath.Join(root, name), 64<<10)
		clear(raw)
	}
	store, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: root})
	if err != nil {
		t.Fatal("existing identity could not be opened")
	}
	registration, err := store.Registration()
	if err != nil || registration.AccountID == "" {
		t.Fatal("existing registration is invalid")
	}
	if _, err := store.MachineControl(time.Now().UTC(), 0); err != nil {
		t.Fatal("existing machine-control credential is unavailable")
	}
	base, err := url.Parse(registration.ServerURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		t.Fatal("existing control origin is invalid")
	}
	keys, err := productionEnvironmentKeySourceForState(root, registration)
	if err != nil {
		t.Fatal("existing ENV custody is unavailable")
	}
	marker, ok := keys.(environmentkey.LayerGenesisMarker)
	if !ok {
		t.Fatal("existing ENV genesis authority is unavailable")
	}
	established, err := marker.LayerGenesisEstablished()
	if err != nil || !established {
		t.Fatal("existing established ENV genesis is required")
	}
	floor := readExisting(filepath.Join(root, "environment", "layer-high-water.json"), (8<<20)+256)
	defer clear(floor)
	// This isolated floor must never be handed to an older installed owner.
	// The final upgraded owner consumes and acknowledges from its own baseline.
	cachePath := os.Getenv("PAPERBOAT_ENV_LIVE_FLOOR_OUTPUT")
	if !filepath.IsAbs(cachePath) || filepath.Clean(cachePath) == filepath.Join(root, "environment", "layer-high-water.json") {
		t.Fatal("a distinct task-owned encrypted floor output is required")
	}
	if os.Getenv("PAPERBOAT_ENV_LIVE_FLOOR_REUSE") != "1" {
		output, err := os.OpenFile(cachePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("task-owned floor output must be new and writable")
		}
		if err := output.Close(); err != nil {
			t.Fatal("isolated floor reservation could not close")
		}
		if err := atomicfile.Write(cachePath, floor, atomicfile.CurrentOwnerOptions(0600)); err != nil {
			t.Fatal("isolated encrypted floor could not be protected and stored")
		}
	}
	if os.Getenv("PAPERBOAT_ENV_LIVE_KEEP_FLOOR") != "1" {
		t.Cleanup(func() {
			if err := os.Remove(cachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error("isolated encrypted floor cleanup failed")
			}
		})
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	probe := &liveRecordTransport{base: transport, observationPath: "/v1/environment/hosts/" + url.PathEscape(registration.MachineID) + "/layer-observations"}
	credentials := renewingMachineIdentity{tokens: enrollment.TokenSource{StateRoot: root}, proofs: enrollment.ProofSource{StateRoot: root}}
	observation, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: base.String(), StateRoot: root, Transport: transport})
	if err != nil {
		t.Fatal("production observation transport is unavailable")
	}
	service := newLayerEnvironmentService(root, base, probe, registration, credentials, observation)
	service.layers, err = envinject.NewLayerStore(envinject.LayerConfig{Path: cachePath, Issuer: strings.TrimRight(base.String(), "/"), AccountID: registration.AccountID, MachineID: registration.MachineID, InstallationGeneration: uint64(registration.InstallationGeneration), Keys: keys, Marker: marker, Records: service})
	if err != nil {
		t.Fatal("isolated authenticated floor is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pending, err := service.layers.PendingObservations(ctx)
	if err != nil {
		t.Fatal("isolated existing observations are invalid")
	}
	t.Logf("existing pending observations retained in isolated copy: %d", len(pending))
	values, err := service.EnvironmentForLaunch(envinject.WithLaunchContext(ctx, workspace, actor))
	if err != nil {
		t.Fatal("production ENV launch qualification failed (private cause suppressed)")
	}
	pending, err = service.layers.PendingObservations(ctx)
	if err != nil || len(pending) == 0 || probe.observations.Load() == 0 || probe.recordReads.Load() == 0 {
		t.Fatal("real record reads and deliberately deferred observations are required")
	}
	actual := make(map[string]string, len(values))
	for _, entry := range values {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatal("invalid production ENV result")
		}
		hash := sha256.Sum256([]byte(value))
		actual[name] = hex.EncodeToString(hash[:])
	}
	if len(actual) != len(expected) {
		t.Fatal("production ENV name inventory differs")
	}
	names := make([]string, 0, len(expected))
	for name, hash := range expected {
		if actual[name] != hash {
			t.Fatalf("ENV hash mismatch for %s", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("%s sha256=%s", name, actual[name])
	}
}

// Keep the real network/custody boundary while protecting installed sequence ownership.
type liveRecordTransport struct {
	base            http.RoundTripper
	observationPath string
	observations    atomic.Uint64
	recordReads     atomic.Uint64
}

func (p *liveRecordTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && r.URL.Path == p.observationPath {
		p.observations.Add(1)
		return nil, errors.New("connected qualification deliberately defers installed-owner observations")
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/environment/layers/") && strings.HasSuffix(r.URL.Path, "/records") {
		p.recordReads.Add(1)
	}
	return p.base.RoundTrip(r)
}
