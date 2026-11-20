package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// regionRequest is a request to the region routes as a signed-in account, answered by handlers whose service has no
// database: a request that reached the service would be refused there as unauthorized (403), so a 400 proves the handler
// refused it first.
func regionRequest(t *testing.T, method, target, contentType, body, path string) (*httptest.ResponseRecorder, ErrorResponse) {
	t.Helper()
	request, _, _ := diagnosticRequest(target)
	request = request.Clone(request.Context())
	request.Method = method
	request.Body = httptest.NewRequest(method, target, strings.NewReader(body)).Body
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_regions_01"}}))
	if path != "" {
		request.SetPathValue("regionID", path)
	}
	response := httptest.NewRecorder()
	(&SelfhostHandlers{Service: selfhost.New(nil, strings.Repeat("k", 32))}).Account(response, request)
	var parsed ErrorResponse
	_ = json.Unmarshal(response.Body.Bytes(), &parsed)
	return response, parsed
}

func TestSelfhostRegionWritesAreStrict(t *testing.T) {
	const json = "application/json"
	for name, tc := range map[string]struct{ method, target, contentType, body, region string }{
		"unknown field on create":     {"POST", "/v1/selfhost/regions", json, `{"name":"Home lab","slug":"home-lab"}`, ""},
		"unknown field on rename":     {"PATCH", "/v1/selfhost/regions/home-lab", json, `{"name":"Home lab","id":"x"}`, "home-lab"},
		"unknown scope field":         {"POST", "/v1/selfhost/regions", json, `{"name":"Home lab","scope":{"kind":"account","owner":"x"}}`, ""},
		"trailing data":               {"POST", "/v1/selfhost/regions", json, `{"name":"Home lab"} {"name":"Again"}`, ""},
		"not an object":               {"POST", "/v1/selfhost/regions", json, `["Home lab"]`, ""},
		"empty body":                  {"POST", "/v1/selfhost/regions", json, ``, ""},
		"wrong content type":          {"POST", "/v1/selfhost/regions", "text/plain", `{"name":"Home lab"}`, ""},
		"no content type":             {"PATCH", "/v1/selfhost/regions/home-lab", "", `{"name":"Home lab"}`, "home-lab"},
		"query and body scope differ": {"POST", "/v1/selfhost/regions?scope_kind=team&scope_id=t1", json, `{"name":"Home lab","scope":{"kind":"account"}}`, ""},
		"unknown query on list":       {"GET", "/v1/selfhost/regions?limit=5", "", "", ""},
		"repeated query":              {"GET", "/v1/selfhost/regions?scope_kind=team&scope_kind=account", "", "", ""},
	} {
		response, body := regionRequest(t, tc.method, tc.target, tc.contentType, tc.body, tc.region)
		if response.Code != http.StatusBadRequest || body.Error.Code != "invalid_request" {
			t.Errorf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
}

func TestSelfhostRegionRoutesNeedASignedInAccount(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/selfhost/regions", nil)
	response := httptest.NewRecorder()
	(&SelfhostHandlers{Service: selfhost.New(nil, strings.Repeat("k", 32))}).Account(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSelfhostRegionRoutesAreNotOperatorRoutes(t *testing.T) {
	request, _, _ := diagnosticRequest("/v1/admin/selfhost/regions")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "operator_regions_01"}}))
	response := httptest.NewRecorder()
	(&SelfhostHandlers{Service: selfhost.New(nil, strings.Repeat("k", 32))}).Global(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("the operator route served regions: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSelfhostRegionErrorsMapToCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("A region called %q already exists.: %w", "Home lab", selfhost.ErrRegionExists), 409, "region_exists"},
		{fmt.Errorf("Home lab still has machines. Remove them first.: %w", selfhost.ErrRegionInUse), 409, "region_in_use"},
		{selfhost.ErrRegionNotFound, 404, "region_not_found"},
		{fmt.Errorf("limit: %w", selfhost.ErrLimit), 429, "selfhost_limit"},
		{selfhost.ErrUnauthorized, 403, "installation_unauthorized"},
		{selfhost.ErrInvalid, 400, "invalid_request"},
		{errors.New("database down"), 503, "selfhost_unavailable"},
	} {
		request, _, _ := diagnosticRequest("/v1/selfhost/regions")
		response := httptest.NewRecorder()
		selfhostError(response, request, tc.err)
		var body ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != tc.status || body.Error.Code != tc.code {
			t.Errorf("%v: status=%d code=%s", tc.err, response.Code, body.Error.Code)
		}
	}
}
