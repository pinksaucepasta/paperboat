package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

type edgeInventoryStub struct {
	account string
	after   string
	limit   int
	err     error
}

func (s *edgeInventoryStub) ListHostedEdges(_ context.Context, account, after string, limit int) (controlplane.HostedEdgePage, error) {
	s.account, s.after, s.limit = account, after, limit
	return controlplane.HostedEdgePage{Items: []controlplane.HostedEdge{{ID: "edge_1", Region: "fsn1", Status: "ready"}}, ObservedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)}, s.err
}

func TestHostedEdgeListBindsAccountAndReturnsPublicFields(t *testing.T) {
	stub := &edgeInventoryStub{}
	request := httptest.NewRequest(http.MethodGet, "/v1/edges?after=edge_0&limit=1", nil)
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}}))
	response := httptest.NewRecorder()
	hostedEdgeList(stub).ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.account != "acct_1" || stub.after != "edge_0" || stub.limit != 1 {
		t.Fatalf("code=%d account=%q after=%q limit=%d", response.Code, stub.account, stub.after, stub.limit)
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Body.String(), "credential") || !strings.Contains(response.Body.String(), "edge_1") {
		t.Fatalf("unexpected edge response: %s", response.Body.String())
	}
}

func TestHostedEdgeListRejectsMissingAuthAndInvalidLimit(t *testing.T) {
	stub := &edgeInventoryStub{}
	for _, testcase := range []struct {
		url  string
		auth bool
		want int
	}{{"/v1/edges", false, 401}, {"/v1/edges?limit=101", true, 400}} {
		request := httptest.NewRequest(http.MethodGet, testcase.url, nil)
		if testcase.auth {
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}}))
		}
		response := httptest.NewRecorder()
		hostedEdgeList(stub).ServeHTTP(response, request)
		if response.Code != testcase.want || stub.account != "" {
			t.Fatalf("url=%q code=%d account=%q", testcase.url, response.Code, stub.account)
		}
	}
	stub.err = errors.New("database offline")
	request := httptest.NewRequest(http.MethodGet, "/v1/edges", nil)
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}}))
	response := httptest.NewRecorder()
	hostedEdgeList(stub).ServeHTTP(response, request)
	if response.Code != 503 || strings.Contains(response.Body.String(), "database offline") {
		t.Fatalf("unavailable response: %d %s", response.Code, response.Body.String())
	}
}

func TestHostedEdgeListRequiresTunnelReadScope(t *testing.T) {
	stub := &edgeInventoryStub{}
	request := httptest.NewRequest(http.MethodGet, "/v1/edges", nil)
	client := auth.ClientPrincipal{SessionID: "cli_1", Scopes: []string{"machines:read"}}
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "acct_1"}, Client: &client}))
	response := httptest.NewRecorder()
	requireScope("tunnels:read", hostedEdgeList(stub)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || stub.account != "" {
		t.Fatalf("scope rejection: %d account=%q", response.Code, stub.account)
	}
}
