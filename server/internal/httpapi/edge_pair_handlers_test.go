package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

type edgePairStub struct {
	actor, kind, primary, fallback string
	scope                          selfhost.Scope
	err                            error
}

func (s *edgePairStub) pair() selfhost.EdgePair {
	return selfhost.EdgePair{Kind: "managed", Primary: selfhost.PairRegion{ID: "frankfurt", Name: "Frankfurt", Status: "ready"}}
}

func (s *edgePairStub) EdgePair(_ context.Context, actor string, scope selfhost.Scope) (selfhost.EdgePair, error) {
	s.actor, s.scope = actor, scope
	return s.pair(), s.err
}

func (s *edgePairStub) SetEdgePair(_ context.Context, actor string, scope selfhost.Scope, kind, primary, fallback string) (selfhost.EdgePair, error) {
	s.actor, s.scope, s.kind, s.primary, s.fallback = actor, scope, kind, primary, fallback
	return s.pair(), s.err
}

func edgePairRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	return request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}}))
}

func TestEdgePairRoutesBindAccountAndWorkspace(t *testing.T) {
	stub := &edgePairStub{}
	response := httptest.NewRecorder()
	edgePairGet(stub).ServeHTTP(response, edgePairRequest(http.MethodGet, "/v1/edges/pair", ""))
	if response.Code != http.StatusOK || stub.actor != "acct_1" || stub.scope != (selfhost.Scope{Kind: "account", ID: "acct_1"}) || !strings.Contains(response.Body.String(), `"data":{"kind":"managed","primary":{"id":"frankfurt","name":"Frankfurt","status":"ready"}}`) || strings.Contains(response.Body.String(), "fallback") {
		t.Fatalf("personal get: code=%d scope=%+v body=%s", response.Code, stub.scope, response.Body.String())
	}
	response = httptest.NewRecorder()
	edgePairPut(stub).ServeHTTP(response, edgePairRequest(http.MethodPut, "/v1/edges/pair?workspace=acme", `{"kind":"self_hosted","primary_region":"a","fallback_region":"b"}`))
	if response.Code != http.StatusOK || stub.scope != (selfhost.Scope{Kind: "team", ID: "acme"}) || stub.kind != "self_hosted" || stub.primary != "a" || stub.fallback != "b" {
		t.Fatalf("team put: code=%d scope=%+v %q %q %q", response.Code, stub.scope, stub.kind, stub.primary, stub.fallback)
	}
}

func TestEdgePairErrorsAreTheDocumentedStatuses(t *testing.T) {
	for _, item := range []struct {
		name   string
		err    error
		status int
		code   string
		count  string
	}{
		{"running", fmt.Errorf("switch: %w", &selfhost.LivePublicationsError{Count: 3}), http.StatusConflict, "previews_running", `"count":3`},
		{"running names", &selfhost.LivePublicationsError{Count: 1, Items: []selfhost.LiveItem{{Kind: "tunnel", ID: "t1", Name: "api tunnel"}}}, http.StatusConflict, "previews_running", `"items":[{"kind":"tunnel","id":"t1","name":"api tunnel"}]`},
		{"forbidden", selfhost.ErrUnauthorized, http.StatusForbidden, "forbidden", ""},
		{"invalid", fmt.Errorf("%w: same edge", selfhost.ErrInvalid), http.StatusBadRequest, "invalid_request", ""},
		{"region named", fmt.Errorf("switch: %w", &selfhost.InvalidPairError{Message: "Mumbai is not available to this workspace right now; choose a region that is ready."}), http.StatusBadRequest, "invalid_request", `"message":"Mumbai is not available to this workspace right now; choose a region that is ready."`},
		{"unassigned", selfhost.ErrEdgePairUnassigned, http.StatusNotFound, "edge_pair_unassigned", ""},
		{"down", errors.New("database down"), http.StatusServiceUnavailable, "edge_pair_unavailable", ""},
	} {
		response := httptest.NewRecorder()
		edgePairPut(&edgePairStub{err: item.err}).ServeHTTP(response, edgePairRequest(http.MethodPut, "/v1/edges/pair", `{"kind":"managed","primary_region":"a","fallback_region":"b"}`))
		if response.Code != item.status || !strings.Contains(response.Body.String(), `"code":"`+item.code+`"`) || !strings.Contains(response.Body.String(), item.count) {
			t.Fatalf("%s: code=%d body=%s", item.name, response.Code, response.Body.String())
		}
	}
	for name, body := range map[string]string{"unknown field": `{"kind":"managed","primary_region":"a","fallback_region":"b","mode":"x"}`, "old ids": `{"primary_id":"a","fallback_id":"b"}`, "not json": `x`, "two objects": `{"kind":"managed","primary_region":"a","fallback_region":"b"}{}`} {
		response := httptest.NewRecorder()
		edgePairPut(&edgePairStub{}).ServeHTTP(response, edgePairRequest(http.MethodPut, "/v1/edges/pair", body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d", name, response.Code)
		}
	}
	response := httptest.NewRecorder()
	edgePairGet(&edgePairStub{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/edges/pair", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: code=%d", response.Code)
	}
}
