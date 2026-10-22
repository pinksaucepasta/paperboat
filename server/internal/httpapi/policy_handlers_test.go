package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

type fakePolicyAPI struct{}

func (fakePolicyAPI) Policy(context.Context, string, string, uint64) (teams.PolicyView, error) {
	return teams.PolicyView{Workspace: "personal", Version: 3, Policy: "{}", CanEdit: "all"}, nil
}
func (fakePolicyAPI) ValidatePolicy(context.Context, string, string, []byte) (teams.PolicyValidation, error) {
	return teams.PolicyValidation{Valid: true}, nil
}
func (fakePolicyAPI) SavePolicy(context.Context, string, string, teams.PolicySaveRequest) (teams.PolicyView, error) {
	return teams.PolicyView{}, nil
}
func (fakePolicyAPI) RestorePolicy(context.Context, string, string, teams.PolicyRestoreRequest) (teams.PolicyView, error) {
	return teams.PolicyView{}, nil
}
func (fakePolicyAPI) ExplainPolicy(context.Context, string, string, teams.PolicyExplainRequest) (teams.PolicyExplanation, error) {
	return teams.PolicyExplanation{}, nil
}
func (fakePolicyAPI) PolicyVersions(context.Context, string, string) ([]teams.PolicyVersion, error) {
	return []teams.PolicyVersion{{Version: 3, Kind: "saved"}}, nil
}

// Policy responses use the data envelope every client (pb, console) reads; a bare object made
// every `pb policy` command fail with "returned an empty response" on dev-e7c2da9c.
func TestPolicyResponsesUseTheDataEnvelope(t *testing.T) {
	mux := http.NewServeMux()
	pass := func(h http.Handler) http.Handler { return h }
	registerPolicyRoutes(mux, fakePolicyAPI{}, pass, pass)
	for _, path := range []string{"/v1/workspaces/personal/policy", "/v1/workspaces/personal/policy/versions"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "u1"}}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var body struct {
			Data map[string]any `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Data == nil {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/policy/schema", nil))
	var schema map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &schema) != nil || schema["$schema"] == nil {
		t.Fatal("the schema endpoint serves the JSON Schema document itself, for editors")
	}
}
