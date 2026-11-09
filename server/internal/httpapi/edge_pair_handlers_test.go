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
	actor, primary, fallback string
	scope                    selfhost.Scope
	err                      error
}

func (s *edgePairStub) pair() selfhost.EdgePair {
	return selfhost.EdgePair{Primary: selfhost.PairEdge{ID: "edge_1", Kind: "managed", Region: "fsn1", Status: "ready"}}
}

func (s *edgePairStub) EdgePair(_ context.Context, actor string, scope selfhost.Scope) (selfhost.EdgePair, error) {
	s.actor, s.scope = actor, scope
	return s.pair(), s.err
}

func (s *edgePairStub) SetEdgePair(_ context.Context, actor string, scope selfhost.Scope, primary, fallback string) (selfhost.EdgePair, error) {
	s.actor, s.scope, s.primary, s.fallback = actor, scope, primary, fallback
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
	if response.Code != http.StatusOK || stub.actor != "acct_1" || stub.scope != (selfhost.Scope{Kind: "account", ID: "acct_1"}) || !strings.Contains(response.Body.String(), `"data":{"primary":{"id":"edge_1"`) || strings.Contains(response.Body.String(), "fallback") {
		t.Fatalf("personal get: code=%d scope=%+v body=%s", response.Code, stub.scope, response.Body.String())
	}
	response = httptest.NewRecorder()
	edgePairPut(stub).ServeHTTP(response, edgePairRequest(http.MethodPut, "/v1/edges/pair?workspace=acme", `{"primary_id":"a","fallback_id":"b"}`))
	if response.Code != http.StatusOK || stub.scope != (selfhost.Scope{Kind: "team", ID: "acme"}) || stub.primary != "a" || stub.fallback != "b" {
		t.Fatalf("team put: code=%d scope=%+v %q %q", response.Code, stub.scope, stub.primary, stub.fallback)
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
		{"forbidden", selfhost.ErrUnauthorized, http.StatusForbidden, "forbidden", ""},
		{"invalid", fmt.Errorf("%w: same edge", selfhost.ErrInvalid), http.StatusBadRequest, "invalid_request", ""},
		{"unassigned", selfhost.ErrEdgePairUnassigned, http.StatusNotFound, "edge_pair_unassigned", ""},
		{"down", errors.New("database down"), http.StatusServiceUnavailable, "edge_pair_unavailable", ""},
	} {
		response := httptest.NewRecorder()
		edgePairPut(&edgePairStub{err: item.err}).ServeHTTP(response, edgePairRequest(http.MethodPut, "/v1/edges/pair", `{"primary_id":"a","fallback_id":"b"}`))
		if response.Code != item.status || !strings.Contains(response.Body.String(), `"code":"`+item.code+`"`) || !strings.Contains(response.Body.String(), item.count) {
			t.Fatalf("%s: code=%d body=%s", item.name, response.Code, response.Body.String())
		}
	}
	for name, body := range map[string]string{"unknown field": `{"primary_id":"a","fallback_id":"b","mode":"x"}`, "not json": `x`, "two objects": `{"primary_id":"a","fallback_id":"b"}{}`} {
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
