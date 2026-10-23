package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestBindWorkspaceSelectorRequiresSignedBodyToMatchQuery(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/tunnels?workspace=team-a", nil)
	var bodyScope = "team-a"
	workspaceQueryContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bound, scope, selected, err := BindWorkspaceSelector(r, &bodyScope, true)
		if err != nil || !selected || scope.TeamID != "team-a" {
			t.Fatalf("matching selector: scope=%+v selected=%t err=%v", scope, selected, err)
		}
		if got, ok := workspace.FromContext(bound.Context()); !ok || got != scope {
			t.Fatalf("bound context = %+v, %t; want %+v", got, ok, scope)
		}
	})).ServeHTTP(httptest.NewRecorder(), request)
}

func TestBindWorkspaceSelectorRejectsQueryOnlyAndMismatch(t *testing.T) {
	for _, test := range []struct {
		name       string
		url        string
		body       *string
		wantErr    error
		wantSet    bool
		wantTeamID string
	}{
		{name: "query only signed create", url: "/v1/tunnels?workspace=team-a", wantErr: workspace.ErrInvalid},
		{name: "mismatch", url: "/v1/tunnels?workspace=team-a", body: stringPointer("team-b"), wantErr: workspace.ErrInvalid},
		{name: "body only", url: "/v1/tunnels", body: stringPointer("team-b"), wantSet: true, wantTeamID: "team-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", test.url, nil)
			workspaceQueryContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, scope, selected, err := BindWorkspaceSelector(r, test.body, true)
				if (test.wantErr == nil && err != nil) || (test.wantErr != nil && !errors.Is(err, test.wantErr)) {
					t.Fatalf("BindWorkspaceSelector error = %v, want %v", err, test.wantErr)
				}
				if selected != test.wantSet {
					t.Fatalf("selected = %t, want %t", selected, test.wantSet)
				}
				if scope.TeamID != test.wantTeamID {
					t.Fatalf("scope team = %q, want %q", scope.TeamID, test.wantTeamID)
				}
			})).ServeHTTP(httptest.NewRecorder(), request)
		})
	}
}

func TestWorkspaceTargetRejectsCrossWorkspaceSelector(t *testing.T) {
	for _, test := range []struct {
		name       string
		url        string
		target     workspace.Scope
		wantStatus int
	}{
		{name: "team selected personal target", url: "/v1/environment-vault?workspace=team-a", target: workspace.Scope{ID: "personal"}, wantStatus: http.StatusConflict},
		{name: "team selected matching team target", url: "/v1/environment/scopes/team/team-a?workspace=team-a", target: workspace.Scope{ID: "team-a", TeamID: "team-a"}, wantStatus: http.StatusOK},
		{name: "resource bound without selector", url: "/v1/environment-vault", target: workspace.Scope{ID: "personal"}, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.url, nil)
			response := httptest.NewRecorder()
			called := false
			workspaceQueryContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !requireWorkspaceTarget(w, r, test.target) {
					return
				}
				called = true
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if called != (test.wantStatus == http.StatusOK) {
				t.Fatalf("target handler called = %t for status %d", called, response.Code)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func TestPublisherListsValidateWorkspaceBeforeService(t *testing.T) {
	for _, endpoint := range []string{"previews", "tunnels"} {
		for _, test := range []struct {
			name, query, code string
			status            int
		}{
			{"invalid", "?workspace=Bad%20Team", "invalid_workspace", http.StatusBadRequest},
			{"repeated", "?workspace=alpha&workspace=alpha", "invalid_workspace", http.StatusBadRequest},
			{"unavailable", "?workspace=deleted-team", "workspace_unavailable", http.StatusForbidden},
			{"unselected", "", "", http.StatusOK},
		} {
			t.Run(endpoint+"/"+test.name, func(t *testing.T) {
				previews, tunnels := &fakePreviewLeaseAPI{}, &tunnelAPIStub{}
				var handler http.Handler = previewLeaseList(previews)
				if endpoint == "tunnels" {
					handler = tunnelList(tunnels)
				}
				// A missing workspace authority service must fail closed for a
				// selected team, just as a real service denying revoked membership.
				handler = workspaceQueryContext(requireWorkspaceSelection(nil, handler))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, tunnelHandlerRequest(http.MethodGet, "/v1/"+endpoint+test.query, nil))
				if response.Code != test.status || (test.code != "" && !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`)) {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				calls := previews.listCalls + tunnels.listCalls
				if (test.status == http.StatusOK && calls != 1) || (test.status != http.StatusOK && calls != 0) {
					t.Fatalf("service list calls=%d", calls)
				}
			})
		}
	}
}

func TestWorkspaceErrorMapsOnlyPureExpectedFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "pure invalid", err: workspace.ErrInvalid, wantStatus: http.StatusBadRequest},
		{name: "wrapped invalid", err: fmt.Errorf("selector parse: %w", workspace.ErrInvalid), wantStatus: http.StatusBadRequest},
		{name: "pure unavailable", err: workspace.ErrForbidden, wantStatus: http.StatusForbidden},
		{name: "repeated unavailable leaf", err: errors.Join(workspace.ErrForbidden, workspace.ErrForbidden), wantStatus: http.StatusForbidden},
		{name: "unavailable with storage failure", err: errors.Join(workspace.ErrForbidden, syscall.EIO, errors.New("WORKSPACE_SECRET_CANARY")), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/workspaces?workspace=team-a")
			response := httptest.NewRecorder()
			writeWorkspaceError(response, request, test.err)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if test.wantStatus < 500 {
				return
			}
			for _, value := range []string{`"stage":"http_handler"`, `"code":"internal_error"`, `"http_status":500`, reference, "correlation_fault_01"} {
				if !strings.Contains(logs.String(), value) {
					t.Errorf("failure log omitted %q: %s", value, logs.String())
				}
			}
			if strings.Contains(logs.String(), "WORKSPACE_SECRET_CANARY") || strings.Contains(response.Body.String(), "WORKSPACE_SECRET_CANARY") {
				t.Fatal("workspace operational failure exposed arbitrary cause text")
			}
		})
	}
}
