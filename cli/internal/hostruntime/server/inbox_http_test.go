package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

type capabilitiesOff struct{}

func (capabilitiesOff) Enabled(string) bool { return false }

func inboxFixture(t *testing.T, claimsFor func(id, class string) auth.Claims) (http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report final.pdf"), []byte("hello inbox"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := NewInboxHandler(InboxHandlerConfig{
		MachineID: "machine_1", InboxPath: func() (string, error) { return root, nil },
		Authorizer: func(token string) (Authorizer, error) {
			if token != "single-use-proof" {
				return nil, errors.New("unknown token")
			}
			return authorizerFunc(func(_ context.Context, frame protocol.Frame) (Authorization, error) {
				class := map[string]string{inboxReadCapability: "inbox_read", inboxDeleteCapability: "inbox_delete", inboxListCapability: "inbox_list"}[frame.Capability]
				if class == "" || frame.OperationID == "" {
					t.Fatalf("frame = %+v", frame)
				}
				return Authorization{MachineID: "machine_1", Value: claimsFor(inbox.FileID("report final.pdf"), class)}, nil
			}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, root
}

func goodInboxClaims(id, class string) auth.Claims {
	claims := auth.Claims{CredentialClass: class, Subject: "actor_1", ActorID: "actor_1", UserID: "actor_1", AccountID: "account_1", MachineID: "machine_1", ResourceKind: "inbox_file", ResourceID: id, OperationID: "operation_inbox_1", ExpiresAt: 4102444800}
	if class == "inbox_list" {
		claims.ResourceKind, claims.ResourceID = "inbox", "machine_1"
	}
	return claims
}

func inboxRequest(method, id string) *http.Request {
	path := inboxFilesPath + id
	if id == "" {
		path = inboxListPath
	}
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer single-use-proof")
	request.Header.Set(inboxOperationHeader, "operation_inbox_1")
	return request
}

func TestInboxHandlerStreamsTheFileBoundByTheCredential(t *testing.T) {
	handler, _ := inboxFixture(t, goodInboxClaims)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, inboxRequest(http.MethodGet, inbox.FileID("report final.pdf")))
	body, _ := io.ReadAll(recorder.Result().Body)
	if recorder.Code != http.StatusOK || string(body) != "hello inbox" || recorder.Header().Get("Content-Length") != "11" ||
		recorder.Header().Get("Content-Type") != "application/octet-stream" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" ||
		recorder.Header().Get("Content-Disposition") != "attachment; filename*=UTF-8''report%20final.pdf" {
		t.Fatalf("status=%d headers=%v body=%q", recorder.Code, recorder.Header(), body)
	}
}

func TestInboxHandlerDeletesOneFile(t *testing.T) {
	handler, root := inboxFixture(t, goodInboxClaims)
	id := inbox.FileID("report final.pdf")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, inboxRequest(http.MethodDelete, id))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "report final.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still present: %v", err)
	}
	again := httptest.NewRecorder()
	handler.ServeHTTP(again, inboxRequest(http.MethodDelete, id))
	if again.Code != http.StatusNotFound || !strings.Contains(again.Body.String(), "inbox_file_not_found") {
		t.Fatalf("second delete: %d %s", again.Code, again.Body.String())
	}
}

func TestInboxHandlerRefusesAnythingTheCredentialDoesNotBind(t *testing.T) {
	id := inbox.FileID("report final.pdf")
	for name, claims := range map[string]func(string, string) auth.Claims{
		"other file": func(_, class string) auth.Claims { c := goodInboxClaims(inbox.FileID("other"), class); return c },
		"other operation": func(id, class string) auth.Claims {
			c := goodInboxClaims(id, class)
			c.OperationID = "operation_other_9"
			return c
		},
		"other machine": func(id, class string) auth.Claims {
			c := goodInboxClaims(id, class)
			c.MachineID = "machine_2"
			return c
		},
		"wrong class": func(id, _ string) auth.Claims { return goodInboxClaims(id, "preview_launch") },
		"wrong resource": func(id, class string) auth.Claims {
			c := goodInboxClaims(id, class)
			c.ResourceKind = "tunnel"
			return c
		},
		"no actor": func(id, class string) auth.Claims { c := goodInboxClaims(id, class); c.ActorID = ""; return c },
	} {
		handler, root := inboxFixture(t, claims)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, inboxRequest(http.MethodDelete, id))
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d", name, recorder.Code)
		}
		if _, err := os.Stat(filepath.Join(root, "report final.pdf")); err != nil {
			t.Errorf("%s: file removed: %v", name, err)
		}
	}
}

func TestInboxHandlerRejectsMalformedRequests(t *testing.T) {
	handler, _ := inboxFixture(t, goodInboxClaims)
	id := inbox.FileID("report final.pdf")
	missingToken := inboxRequest(http.MethodGet, id)
	missingToken.Header.Del("Authorization")
	badOperation := inboxRequest(http.MethodGet, id)
	badOperation.Header.Set(inboxOperationHeader, "short")
	wrongToken := inboxRequest(http.MethodGet, id)
	wrongToken.Header.Set("Authorization", "Bearer something-else")
	for name, tc := range map[string]struct {
		request *http.Request
		status  int
	}{
		"bad id":          {inboxRequest(http.MethodGet, "../etc/passwd"), http.StatusNotFound},
		"post":            {inboxRequest(http.MethodPost, id), http.StatusMethodNotAllowed},
		"no bearer":       {missingToken, http.StatusUnauthorized},
		"unknown token":   {wrongToken, http.StatusUnauthorized},
		"bad operation":   {badOperation, http.StatusBadRequest},
		"unknown file id": {inboxRequest(http.MethodGet, inbox.FileID("missing")), http.StatusForbidden},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, tc.request)
		if recorder.Code != tc.status {
			t.Errorf("%s: status=%d want %d", name, recorder.Code, tc.status)
		}
	}
}

func TestInboxHandlerHonoursTheCapabilityGate(t *testing.T) {
	root := t.TempDir()
	handler, err := NewInboxHandler(InboxHandlerConfig{
		MachineID: "machine_1", InboxPath: func() (string, error) { return root, nil }, Capabilities: capabilitiesOff{},
		Authorizer: func(string) (Authorizer, error) {
			return authorizerFunc(func(_ context.Context, frame protocol.Frame) (Authorization, error) {
				return Authorization{MachineID: "machine_1", Value: goodInboxClaims(inbox.FileID("a"), "inbox_read")}, nil
			}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, inboxRequest(http.MethodGet, inbox.FileID("a")))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestInboxHandlerListsTheInboxLive(t *testing.T) {
	handler, root := inboxFixture(t, goodInboxClaims)
	if err := os.WriteFile(filepath.Join(root, "second.txt"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".paperboat-receipts.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, inboxRequest(http.MethodGet, ""))
	var answer struct {
		Total int `json:"total"`
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK || answer.Total != 2 || len(answer.Items) != 2 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
	names := map[string]int64{}
	for _, item := range answer.Items {
		names[item.Name] = item.Size
		if item.ID != inbox.FileID(item.Name) {
			t.Errorf("id of %s = %s", item.Name, item.ID)
		}
	}
	if names["report final.pdf"] != 11 || names["second.txt"] != 1 {
		t.Fatalf("items = %v", names)
	}
}

func TestInboxHandlerListNeedsItsOwnCredential(t *testing.T) {
	for name, claims := range map[string]func(string, string) auth.Claims{
		"read credential": func(id, _ string) auth.Claims { return goodInboxClaims(id, "inbox_read") },
		"other machine": func(id, class string) auth.Claims {
			c := goodInboxClaims(id, class)
			c.ResourceID = "machine_2"
			return c
		},
		"file resource": func(id, class string) auth.Claims {
			c := goodInboxClaims(id, class)
			c.ResourceKind = "inbox_file"
			return c
		},
		"another operation id": func(id, class string) auth.Claims {
			c := goodInboxClaims(id, class)
			c.OperationID = "operation_other_9"
			return c
		},
	} {
		handler, _ := inboxFixture(t, claims)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, inboxRequest(http.MethodGet, ""))
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d", name, recorder.Code)
		}
	}
	handler, _ := inboxFixture(t, goodInboxClaims)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, inboxRequest(http.MethodDelete, ""))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("delete of the collection: status=%d", recorder.Code)
	}
}

func TestInboxDispositionNameEncodesForAnExtendedFilename(t *testing.T) {
	for name, want := range map[string]string{
		"report final.pdf": "report%20final.pdf",
		"résumé (1).txt":   "r%C3%A9sum%C3%A9%20%281%29.txt",
		"a;b=c'd.txt":      "a%3Bb%3Dc%27d.txt",
	} {
		if got := inboxDispositionName(name); got != want {
			t.Errorf("%q -> %q, want %q", name, got, want)
		}
	}
}
