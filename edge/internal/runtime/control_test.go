package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/auth"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
)

type trustControlSource struct {
	mu       sync.Mutex
	document []byte
	jwks     []byte
	err      error
	calls    chan struct{}
}

func (s *trustControlSource) JWKS(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.jwks...), nil
}

func (s *trustControlSource) DesiredRoutes(context.Context, string) ([]control.RouteAssignment, error) {
	return []control.RouteAssignment{}, nil
}
func (s *trustControlSource) Revocations(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.calls <- struct{}{}:
	default:
	}
	return append([]byte(nil), s.document...), s.err
}

func TestControlDependencyLoadsAndRefreshesRevocationsAtomically(t *testing.T) {
	source := &trustControlSource{document: []byte(`{"jtis":[]}`), calls: make(chan struct{}, 8)}
	var mu sync.Mutex
	current := ""
	dependency := &ControlDependency{Source: source, TrustSource: source, NodeID: "edge", Interval: time.Millisecond, ApplyJWKS: func([]byte) error { return nil }, ApplyTrust: func(document []byte) error {
		if string(document) == "invalid" {
			return errors.New("invalid snapshot")
		}
		mu.Lock()
		current = string(document)
		mu.Unlock()
		return nil
	}}
	if err := dependency.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-source.calls
	source.mu.Lock()
	source.document = []byte(`{"jtis":["revoked"]}`)
	source.mu.Unlock()
	<-source.calls
	time.Sleep(2 * time.Millisecond)
	mu.Lock()
	got := current
	mu.Unlock()
	if got != `{"jtis":["revoked"]}` {
		t.Fatalf("snapshot = %s", got)
	}
	source.mu.Lock()
	source.document = []byte("invalid")
	source.mu.Unlock()
	for dependency.LastError() == nil {
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got = current
	mu.Unlock()
	if got != `{"jtis":["revoked"]}` {
		t.Fatalf("invalid refresh replaced snapshot: %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dependency.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestControlDependencyFailsStartupWithoutValidRevocations(t *testing.T) {
	source := &trustControlSource{document: []byte("invalid"), calls: make(chan struct{}, 1)}
	dependency := &ControlDependency{Source: source, TrustSource: source, NodeID: "edge", Interval: time.Second, ApplyJWKS: func([]byte) error { return nil }, ApplyTrust: func([]byte) error { return errors.New("invalid snapshot") }}
	if err := dependency.Start(context.Background()); err == nil {
		t.Fatal("invalid initial revocations accepted")
	}
}

// A running edge follows a signing-key rotation: the control plane lists the new key next to the old one,
// then withdraws the old one, and an unusable document leaves the last good set (and the revocations) in place.
func TestControlDependencyRefreshesIssuerKeysThroughRotation(t *testing.T) {
	jwks := func(withdrawn string, ids ...string) []byte {
		keys := ""
		for _, id := range ids {
			public, _, _ := ed25519.GenerateKey(rand.Reader)
			keys += `,{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"` + id + `","x":"` + base64.RawURLEncoding.EncodeToString(public) + `"}`
		}
		return []byte(`{"keys":[` + keys[1:] + `]` + withdrawn + `}`)
	}
	source := &trustControlSource{document: []byte(`{"jtis":[],"environments":[],"connector_generations":[],"key_ids":[]}`), jwks: jwks("", "old"), calls: make(chan struct{}, 64)}
	snapshot := auth.NewSnapshot()
	dependency := &ControlDependency{Source: source, TrustSource: source, NodeID: "edge", Interval: time.Millisecond, ApplyTrust: snapshot.ReplaceRevocations, ApplyJWKS: snapshot.ReplaceJWKS}
	if err := dependency.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dependency.Shutdown(context.Background()) }()
	has := func(id string) bool { _, err := snapshot.Key(context.Background(), id); return err == nil }
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	set := func(document []byte) { source.mu.Lock(); source.jwks = document; source.mu.Unlock() }
	if !has("old") || has("new") {
		t.Fatal("startup keys wrong")
	}
	set(jwks("", "old", "new"))
	eventually("new key never trusted during the overlap", func() bool { return has("new") && has("old") })
	set(jwks(`,"withdrawn_key_ids":["old"]`, "old", "new"))
	eventually("withdrawn key still trusted", func() bool { return has("new") && !has("old") })
	set([]byte(`{"keys":[]}`))
	eventually("bad document not reported", func() bool { return dependency.LastError() != nil })
	if !has("new") {
		t.Fatal("a bad document replaced the last good key set")
	}
}
