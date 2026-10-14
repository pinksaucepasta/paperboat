package nodeauth

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"syscall"
	"testing"
)

type resolverFunc struct {
	node, credential string
	self             bool
	nodeErr          error
	credentialErr    error
}

func (r resolverFunc) RuntimeNode(context.Context, string) (string, error) {
	if r.nodeErr != nil {
		return "", r.nodeErr
	}
	if r.node == "" {
		return "", ErrNotFound
	}
	return r.node, nil
}
func (r resolverFunc) RuntimeCredential(context.Context, string) (string, error) {
	if r.credentialErr != nil {
		return "", r.credentialErr
	}
	if r.credential == "" {
		return "", ErrNotFound
	}
	return r.credential, nil
}
func (r resolverFunc) IsSelfHosted(context.Context, string) (bool, error) { return r.self, nil }
func TestCompositeUsesFirstSuccessfulResolver(t *testing.T) {
	c := Composite{resolverFunc{}, resolverFunc{node: "managed", credential: "secret"}}
	if n, e := c.RuntimeNode(t.Context(), "x"); e != nil || n != "managed" {
		t.Fatalf("node=%q err=%v", n, e)
	}
	if _, e := (Composite{}).RuntimeNode(t.Context(), "x"); !errors.Is(e, ErrNotFound) {
		t.Fatalf("missing=%v", e)
	}
}

func TestCompositeFallsThroughOnlyOnPureResolverMiss(t *testing.T) {
	second := resolverFunc{node: "managed", credential: "secret"}
	for _, first := range []resolverFunc{{nodeErr: fmt.Errorf("wrapped: %w", ErrNotFound)}} {
		if node, err := (Composite{first, second}).RuntimeNode(t.Context(), "x"); err != nil || node != "managed" {
			t.Fatalf("pure miss did not fall through: node=%q err=%v", node, err)
		}
	}
	operational := errors.Join(ErrNotFound, syscall.EIO)
	if node, err := (Composite{resolverFunc{nodeErr: operational}, second}).RuntimeNode(t.Context(), "x"); node != "" || !errors.Is(err, syscall.EIO) {
		t.Fatalf("mixed resolver miss hid operational cause: node=%q err=%v", node, err)
	}
	if credential, err := (Composite{resolverFunc{credentialErr: operational}, second}).RuntimeCredential(t.Context(), "x"); credential != "" || !errors.Is(err, syscall.EIO) {
		t.Fatalf("mixed credential miss hid operational cause: credential=%q err=%v", credential, err)
	}
}

func TestMatchesPreservesOperationalResolverError(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer secret")
	resolver := resolverFunc{nodeErr: errors.Join(ErrNotFound, syscall.ECONNRESET)}
	if matched, err := Matches(t.Context(), resolver, r, "", "node_a"); matched || !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("match result concealed resolver cause: matched=%t err=%v", matched, err)
	}
	resolver = resolverFunc{nodeErr: fmt.Errorf("wrapped: %w", ErrNotFound)}
	if matched, err := Matches(t.Context(), resolver, r, "", "node_a"); matched || err != nil {
		t.Fatalf("pure unknown credential was not an ordinary non-match: matched=%t err=%v", matched, err)
	}
}
