package envinject

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
)

const (
	projectionTestIssuer  = "https://control.example"
	projectionTestAccount = "account_1"
	projectionTestMachine = "machine_1"
)

type projectionFixture struct {
	config       ProjectionConfig
	store        *ProjectionStore
	material     environmentkey.Material
	hostPublic   []byte
	writerSeed   []byte
	writerPublic []byte
}

func newProjectionFixture(t *testing.T) *projectionFixture {
	t.Helper()
	material := environmentkey.Material{Generation: 7}
	copy(material.Private[:], bytes.Repeat([]byte{0x37}, len(material.Private)))
	hostPublic, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	writerSeed := bytes.Repeat([]byte{0x49}, ed25519.SeedSize)
	signer := ed25519.NewKeyFromSeed(writerSeed)
	writerPublic := append([]byte(nil), signer.Public().(ed25519.PublicKey)...)
	clear(signer)

	path := filepath.Join(t.TempDir(), "projection", "cache.json")
	marker := &testGenesisMarker{path: testGenesisMarkerPath(path)}
	prepareTestGenesisMarker(t, marker)
	config := ProjectionConfig{
		Path:                   path,
		HighWaterPath:          path + ".high-water",
		Issuer:                 projectionTestIssuer,
		AccountID:              projectionTestAccount,
		MachineID:              projectionTestMachine,
		InstallationGeneration: 11,
		HostKeyGeneration:      material.Generation,
		HostPublic:             append([]byte(nil), hostPublic[:]...),
		WriterPublic:           append([]byte(nil), writerPublic...),
		Keys:                   staticHostKey{material: material},
		GenesisMarker:          marker,
	}
	store, err := OpenProjection(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &projectionFixture{
		config:       config,
		store:        store,
		material:     material,
		hostPublic:   append([]byte(nil), hostPublic[:]...),
		writerSeed:   writerSeed,
		writerPublic: writerPublic,
	}
	t.Cleanup(func() {
		store.Close()
		fixture.material.Destroy()
		clear(fixture.writerSeed)
		clear(fixture.writerPublic)
		clear(fixture.hostPublic)
	})
	return fixture
}

func projectionBundle(t *testing.T, fixture *projectionFixture, writerSeed []byte, previous environmente2ee.DocumentID, fence, selection, revision uint64, state string, values map[string][]byte) ProjectionBundle {
	t.Helper()
	claims := environmente2ee.HostProjectionClaims{
		Issuer:                 fixture.config.Issuer,
		OwnerAccount:           fixture.config.AccountID,
		MachineID:              fixture.config.MachineID,
		InstallationGeneration: fixture.config.InstallationGeneration,
		HostKeyGeneration:      fixture.config.HostKeyGeneration,
		HostPublic:             append([]byte(nil), fixture.hostPublic...),
		SelectionGeneration:    selection,
		Revision:               revision,
		Previous:               append([]byte(nil), previous[:]...),
		Sources:                []environmente2ee.ProjectionSource{},
		WriterVaultGeneration:  1,
	}
	projection, err := environmente2ee.SealHostProjection(context.Background(), claims, writerSeed, values)
	if err != nil {
		t.Fatal(err)
	}
	writer := projection.Claims.WriterPublic
	bundle := ProjectionBundle{
		Schema:                 ProjectionBundleSchema,
		AccountID:              fixture.config.AccountID,
		MachineID:              fixture.config.MachineID,
		InstallationGeneration: fixture.config.InstallationGeneration,
		HostKeyGeneration:      fixture.config.HostKeyGeneration,
		HostPublic:             base64.RawURLEncoding.EncodeToString(fixture.hostPublic),
		WriterPublic:           base64.RawURLEncoding.EncodeToString(writer),
		FenceGeneration:        fence,
		SelectionGeneration:    selection,
		ProjectionRevision:     revision,
		DocumentID:             projection.ID.String(),
		Envelope:               base64.RawURLEncoding.EncodeToString(projection.Raw),
		State:                  state,
	}
	return bundle
}

func stateProjectionBundle(fixture *projectionFixture, writerPublic []byte, fence, selection, revision uint64, state string) ProjectionBundle {
	digest := sha256.Sum256([]byte(state + ":" + string(rune(fence)) + ":" + string(rune(revision))))
	return ProjectionBundle{
		Schema:                 ProjectionBundleSchema,
		AccountID:              fixture.config.AccountID,
		MachineID:              fixture.config.MachineID,
		InstallationGeneration: fixture.config.InstallationGeneration,
		HostKeyGeneration:      fixture.config.HostKeyGeneration,
		HostPublic:             base64.RawURLEncoding.EncodeToString(fixture.hostPublic),
		WriterPublic:           base64.RawURLEncoding.EncodeToString(writerPublic),
		FenceGeneration:        fence,
		SelectionGeneration:    selection,
		ProjectionRevision:     revision,
		DocumentID:             environmente2ee.DocumentID(digest).String(),
		State:                  state,
	}
}

func applyProjectionReady(t *testing.T, fixture *projectionFixture, values map[string][]byte) ProjectionBundle {
	t.Helper()
	bundle := projectionBundle(t, fixture, fixture.writerSeed, environmente2ee.DocumentID{}, 1, 1, 1, "ready", values)
	if err := fixture.store.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func environmentValues(t *testing.T, store *ProjectionStore, want []string) {
	t.Helper()
	got, err := store.Environment()
	if err != nil {
		t.Fatalf("environment error=%v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment=%q want=%q", got, want)
	}
}

func TestProjectionDecryptsExactValuesAndExplicitReadyEmpty(t *testing.T) {
	fixture := newProjectionFixture(t)
	applyProjectionReady(t, fixture, map[string][]byte{"ZED": []byte("last"), "APP_MODE": []byte("safe")})
	if fixture.store.BindingState() != BindingActive {
		t.Fatalf("ready projection binding state=%d", fixture.store.BindingState())
	}
	environmentValues(t, fixture.store, []string{"APP_MODE=safe", "ZED=last"})

	previous, err := environmente2ee.ParseDocumentID(fixture.store.bundle.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	empty := projectionBundle(t, fixture, fixture.writerSeed, previous, 2, 2, 2, "ready", map[string][]byte{})
	if err := fixture.store.Apply(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	values, err := fixture.store.Environment()
	if err != nil {
		t.Fatalf("explicit ready empty environment error=%v", err)
	}
	if values == nil || len(values) != 0 {
		t.Fatalf("explicit ready empty environment=%q", values)
	}
	if fixture.store.BindingState() != BindingActive {
		t.Fatalf("explicit ready empty binding state=%d", fixture.store.BindingState())
	}
}

func TestProjectionPendingAndRevokedClearAndStayBlockedAfterRestart(t *testing.T) {
	fixture := newProjectionFixture(t)
	applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("safe")})

	pending := stateProjectionBundle(fixture, fixture.writerPublic, 2, 1, 2, "pending")
	if err := fixture.store.Apply(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Environment(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("pending environment error=%v", err)
	}
	restarted, err := OpenProjection(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Environment(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("pending restart environment error=%v", err)
	}

	revoked := stateProjectionBundle(fixture, fixture.writerPublic, 3, 2, 3, "revoked")
	if err := restarted.Apply(context.Background(), revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Environment(); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked environment error=%v", err)
	}
	if restarted.BindingState() != BindingInactive {
		t.Fatalf("revoked binding state=%d", restarted.BindingState())
	}
	restarted.Close()
	revokedRestart, err := OpenProjection(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	defer revokedRestart.Close()
	if _, err := revokedRestart.Environment(); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked restart environment error=%v", err)
	}
	if revokedRestart.BindingState() != BindingInactive {
		t.Fatalf("revoked restart binding state=%d", revokedRestart.BindingState())
	}
}

func TestProjectionRejectsTamperIdentityAndStaleFloors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProjectionBundle, *projectionFixture)
	}{
		{
			name: "tampered_envelope",
			mutate: func(bundle *ProjectionBundle, _ *projectionFixture) {
				raw, _ := base64.RawURLEncoding.Strict().DecodeString(bundle.Envelope)
				raw[len(raw)-1] ^= 0x40
				bundle.Envelope = base64.RawURLEncoding.EncodeToString(raw)
			},
		},
		{
			name: "wrong_host",
			mutate: func(bundle *ProjectionBundle, _ *projectionFixture) {
				bundle.HostPublic = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x71}, 32))
			},
		},
		{
			name: "wrong_generation",
			mutate: func(bundle *ProjectionBundle, _ *projectionFixture) {
				bundle.HostKeyGeneration++
			},
		},
		{
			name: "wrong_writer",
			mutate: func(bundle *ProjectionBundle, _ *projectionFixture) {
				bundle.WriterPublic = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x72}, 32))
			},
		},
		{
			name: "stale_fence",
			mutate: func(bundle *ProjectionBundle, _ *projectionFixture) {
				bundle.FenceGeneration = 0
			},
		},
		{
			name: "stale_revision",
			mutate: func(bundle *ProjectionBundle, _ *projectionFixture) {
				bundle.ProjectionRevision = 0
				bundle.DocumentID = ""
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProjectionFixture(t)
			base := applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("safe")})
			invalid := base
			test.mutate(&invalid, fixture)
			if err := fixture.store.Apply(context.Background(), invalid); err == nil {
				t.Fatal("invalid projection was accepted")
			}
			if _, err := fixture.store.Environment(); !errors.Is(err, ErrNotReady) {
				t.Fatalf("failed projection retained usable environment: %v", err)
			}
		})
	}
}

func TestProjectionBindRequiresHigherSelectionForWriterChange(t *testing.T) {
	fixture := newProjectionFixture(t)
	base := applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("old")})
	previous, err := environmente2ee.ParseDocumentID(base.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	newSeed := bytes.Repeat([]byte{0x53}, ed25519.SeedSize)
	defer clear(newSeed)
	changed := projectionBundle(t, fixture, newSeed, previous, 2, 2, 2, "ready", map[string][]byte{"APP_MODE": []byte("new")})
	if err := fixture.store.Apply(context.Background(), changed); err == nil {
		t.Fatal("ordinary Apply accepted a changed projection writer")
	}
	if _, err := fixture.store.Environment(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("failed ordinary writer change retained values: %v", err)
	}

	rebound := newProjectionFixture(t)
	base = applyProjectionReady(t, rebound, map[string][]byte{"APP_MODE": []byte("old")})
	previous, err = environmente2ee.ParseDocumentID(base.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	changed = projectionBundle(t, rebound, newSeed, previous, 2, 2, 2, "ready", map[string][]byte{"APP_MODE": []byte("new")})
	if err := rebound.store.Bind(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	environmentValues(t, rebound.store, []string{"APP_MODE=new"})
}

func TestProjectionInterruptedCacheWriteCannotResurrectOldReadyState(t *testing.T) {
	fixture := newProjectionFixture(t)
	old := applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("old")})
	previous, err := environmente2ee.ParseDocumentID(old.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	next := projectionBundle(t, fixture, fixture.writerSeed, previous, 2, 2, 2, "ready", map[string][]byte{"APP_MODE": []byte("new"), "APP_REGION": []byte("eu")})
	injected := errors.New("interrupted projection cache write")
	write := fixture.store.write
	fixture.store.write = func(path string, key []byte, value any) error {
		if path == fixture.config.Path {
			return injected
		}
		return write(path, key, value)
	}
	if err := fixture.store.Apply(context.Background(), next); !errors.Is(err, injected) {
		t.Fatalf("cache interruption error=%v", err)
	}
	if _, err := fixture.store.Environment(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("interrupted cache write left old values ready: %v", err)
	}

	restarted, err := OpenProjection(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Environment(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("restart resurrected old ready cache: %v", err)
	}
	if restarted.floor.Revision != next.ProjectionRevision || restarted.floor.DocumentID != next.DocumentID || restarted.floor.State != next.State {
		t.Fatalf("restart floor=%+v want revision=%d document=%s state=%s", restarted.floor, next.ProjectionRevision, next.DocumentID, next.State)
	}
	if err := restarted.Apply(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	environmentValues(t, restarted, []string{"APP_MODE=new", "APP_REGION=eu"})
	if restarted.bundle != next {
		t.Fatal("recovered cache bundle differs from the exact delivered bundle")
	}
	recovered, err := OpenProjection(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.bundle != next {
		t.Fatal("restart changed the recovered bundle")
	}
	environmentValues(t, recovered, []string{"APP_MODE=new", "APP_REGION=eu"})
}

func TestProjectionObservationPersistsAndMissingEstablishedFloorFails(t *testing.T) {
	fixture := newProjectionFixture(t)
	bundle := applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("safe")})
	now := time.Unix(1700000000, 123).UTC()
	first, err := fixture.store.NextObservation(now)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := environmente2ee.KeyIDX25519(fixture.hostPublic)
	if err != nil {
		t.Fatal(err)
	}
	if first.Schema != ProjectionObservationSchema || first.ObservationSeq != 1 || first.HostRecipientKeyID != keyID || first.State != "applied" || first.Projection == nil || first.Projection.Revision != bundle.ProjectionRevision || first.Projection.DocumentID != bundle.DocumentID || !first.ObservedAt.Equal(now) {
		t.Fatalf("first observation=%+v", first)
	}

	restarted, err := OpenProjection(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.NextObservation(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.ObservationSeq != 2 || second.State != "applied" || second.Projection == nil || second.Projection.DocumentID != bundle.DocumentID {
		t.Fatalf("persisted observation=%+v", second)
	}
	restarted.Close()
	if err := os.Remove(fixture.config.HighWaterPath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenProjection(context.Background(), fixture.config); !errors.Is(err, ErrObservationLost) {
		t.Fatalf("missing established high-water error=%v", err)
	}
}

func TestProjectionObservationMapsInternalStatesToServerContract(t *testing.T) {
	fixture := newProjectionFixture(t)
	applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("safe")})
	now := time.Unix(1700000100, 0).UTC()

	observation, err := fixture.store.NextObservation(now)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.store.state != "ready" || observation.State != "applied" || observation.ErrorCode != nil {
		t.Fatalf("ready state mapping internal=%q observation=%+v", fixture.store.state, observation)
	}

	restarted, err := OpenProjection(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	observation, err = restarted.NextObservation(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state != "ready" || observation.State != "applied" || observation.ErrorCode != nil {
		t.Fatalf("restarted ready state mapping internal=%q observation=%+v", restarted.state, observation)
	}

	pending := stateProjectionBundle(fixture, fixture.writerPublic, 2, 2, 2, "pending")
	if err := restarted.Apply(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	observation, err = restarted.NextObservation(now.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state != "pending" || observation.State != "pending" || observation.ErrorCode != nil {
		t.Fatalf("pending state mapping internal=%q observation=%+v", restarted.state, observation)
	}

	revoked := stateProjectionBundle(fixture, fixture.writerPublic, 3, 3, 3, "revoked")
	if err := restarted.Apply(context.Background(), revoked); err != nil {
		t.Fatal(err)
	}
	observation, err = restarted.NextObservation(now.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state != "revoked" || observation.State != "revoked" || observation.ErrorCode != nil {
		t.Fatalf("revoked state mapping internal=%q observation=%+v", restarted.state, observation)
	}

	invalid := stateProjectionBundle(fixture, fixture.writerPublic, 4, 4, 4, "invalid")
	if err := restarted.Apply(context.Background(), invalid); err == nil {
		t.Fatal("invalid projection unexpectedly applied")
	}
	observation, err = restarted.NextObservation(now.Add(4 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state != "error" || observation.State != "failed" || observation.ErrorCode == nil || *observation.ErrorCode != "projection_invalid" {
		t.Fatalf("error state mapping internal=%q observation=%+v", restarted.state, observation)
	}

}

func TestProjectionContextCancellationFailsClosed(t *testing.T) {
	fixture := newProjectionFixture(t)
	applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("safe")})
	previous, err := environmente2ee.ParseDocumentID(fixture.store.bundle.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	next := projectionBundle(t, fixture, fixture.writerSeed, previous, 2, 2, 2, "ready", map[string][]byte{"APP_MODE": []byte("new")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := fixture.store.Apply(ctx, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled projection apply error=%v", err)
	}
	if _, err := fixture.store.Environment(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("canceled apply retained ready values: %v", err)
	}
}

func TestProjectionConcurrentEnvironmentApplyAndObservation(t *testing.T) {
	fixture := newProjectionFixture(t)
	old := applyProjectionReady(t, fixture, map[string][]byte{"APP_MODE": []byte("old")})
	previous, err := environmente2ee.ParseDocumentID(old.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	next := projectionBundle(t, fixture, fixture.writerSeed, previous, 2, 2, 2, "ready", map[string][]byte{"APP_MODE": []byte("new")})

	var wait sync.WaitGroup
	errorsCh := make(chan error, 512)
	wait.Add(1)
	go func() {
		defer wait.Done()
		if err := fixture.store.Apply(context.Background(), next); err != nil {
			errorsCh <- err
		}
	}()
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 16; i++ {
				if _, err := fixture.store.Environment(); err != nil {
					errorsCh <- err
				}
			}
		}()
	}
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for i := 0; i < 16; i++ {
				if _, err := fixture.store.NextObservation(time.Unix(1800000000+int64(worker*100+i), 0)); err != nil {
					errorsCh <- err
				}
			}
		}(worker)
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	environmentValues(t, fixture.store, []string{"APP_MODE=new"})
}
