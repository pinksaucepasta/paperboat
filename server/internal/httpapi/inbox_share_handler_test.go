package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/inboxdispatch"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

func shareHTTP(t *testing.T, fileID, body, key string) *http.Request {
	t.Helper()
	request := inboxHTTP(http.MethodPost, fileID+"/share", true, map[string]string{"Idempotency-Key": key})
	request.SetPathValue("file_id", fileID)
	request.Body = httpBody(body)
	return request
}

func httpBody(body string) *readCloser { return &readCloser{strings.NewReader(body)} }

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }

func shareBody(hours int) string {
	return `{"access_mode":"public","expires_at":"` + time.Now().Add(time.Duration(hours)*time.Hour).UTC().Format(time.RFC3339) + `"}`
}

func TestInboxShareCreatesAFilePreviewOnThatMachine(t *testing.T) {
	inbox := &fakeInboxAPI{listing: inboxdispatch.Listing{Total: 1, Items: []inboxdispatch.Item{{ID: validInboxFile, Name: "report.pdf", Size: 10}}}}
	previews := &fakePreviewLeaseAPI{create: previewv1.CreateResult{
		Preview: previewv1.Preview{ID: "preview_1", Kind: "preview_lease"}, ETag: `"e"`,
		Operation: previewtunnelapi.Operation{ID: "operation_1", State: "succeeded"},
	}}
	handler := userMachineInboxShare(fakeInboxAuthority{}, inbox, previews, 1<<20)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, shareHTTP(t, validInboxFile, shareBody(8), "key-1"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
	var answer struct {
		Data previewv1.Preview `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.Data.ID != "preview_1" || recorder.Header().Get("ETag") != `"e"` {
		t.Fatalf("answer %s (%v)", recorder.Body, err)
	}
	got := previews.createRequest
	if got.OwnerMachineID != "machine_1" || got.OwnerSessionKind != "foreground" || got.Target != (previewv1.Target{Scheme: "file", Address: validInboxFile}) ||
		got.AccessMode != "public" || got.ExpiresAt == nil || got.IdempotencyKey != "key-1" || got.Domains == nil || !strings.HasPrefix(got.OwnerSessionID, "console_") {
		t.Fatalf("create request %+v", got)
	}
	if len(inbox.listed) != 1 || inbox.listed[0].FileID != "" || inbox.listed[0].MachineID != "machine_1" {
		t.Fatalf("listing request %+v", inbox.listed)
	}
	// The same key replays the same create: the owner session does not change between retries.
	first := got.OwnerSessionID
	handler.ServeHTTP(httptest.NewRecorder(), shareHTTP(t, validInboxFile, shareBody(8), "key-1"))
	if previews.createRequest.OwnerSessionID != first {
		t.Fatal("owner session must be derived from the Idempotency-Key")
	}
}

func TestInboxShareRefusals(t *testing.T) {
	listing := inboxdispatch.Listing{Total: 1, Items: []inboxdispatch.Item{{ID: validInboxFile, Name: "big.iso", Size: 2 << 20}}}
	cases := []struct {
		name      string
		authority fakeInboxAuthority
		inbox     *fakeInboxAPI
		createErr error
		fileID    string
		body, key string
		status    int
		code      string
	}{
		{"stranger", fakeInboxAuthority{err: usermachines.ErrNotFound}, &fakeInboxAPI{listing: listing}, nil, validInboxFile, shareBody(1), "k", http.StatusNotFound, "user_machine_not_found"},
		{"malformed id", fakeInboxAuthority{}, &fakeInboxAPI{listing: listing}, nil, "../../etc/passwd", shareBody(1), "k", http.StatusNotFound, "inbox_file_not_found"},
		{"not in inbox", fakeInboxAuthority{}, &fakeInboxAPI{}, nil, validInboxFile, shareBody(1), "k", http.StatusNotFound, "inbox_file_not_found"},
		{"too large", fakeInboxAuthority{}, &fakeInboxAPI{listing: listing}, nil, validInboxFile, shareBody(1), "k", http.StatusRequestEntityTooLarge, "file_too_large"},
		{"offline", fakeInboxAuthority{}, &fakeInboxAPI{listErr: inboxdispatch.ErrMachineOffline}, nil, validInboxFile, shareBody(1), "k", http.StatusConflict, "machine_offline"},
		{"no key", fakeInboxAuthority{}, &fakeInboxAPI{listing: listing}, nil, validInboxFile, shareBody(1), "", http.StatusBadRequest, "idempotency_key_required"},
		{"no expiry", fakeInboxAuthority{}, &fakeInboxAPI{listing: listing}, nil, validInboxFile, `{"access_mode":"public"}`, "k", http.StatusBadRequest, "validation_failed"},
		{"unknown field", fakeInboxAuthority{}, &fakeInboxAPI{listing: listing}, nil, validInboxFile, `{"access_mode":"public","path":"/etc"}`, "k", http.StatusBadRequest, "validation_failed"},
		{"plan limit", fakeInboxAuthority{}, &fakeInboxAPI{listing: inboxdispatch.Listing{Items: []inboxdispatch.Item{{ID: validInboxFile, Size: 1}}}},
			&planlimits.LimitError{Kind: planlimits.Publications, Plan: planlimits.Free, Limit: 1, Used: 1}, validInboxFile, shareBody(1), "k", http.StatusConflict, ""},
		{"owner denied", fakeInboxAuthority{}, &fakeInboxAPI{listing: inboxdispatch.Listing{Items: []inboxdispatch.Item{{ID: validInboxFile, Size: 1}}}},
			previewv1.ErrOwnerDenied, validInboxFile, shareBody(1), "k", http.StatusForbidden, "forbidden"},
	}
	for _, tc := range cases {
		previews := &fakePreviewLeaseAPI{err: tc.createErr}
		recorder := httptest.NewRecorder()
		userMachineInboxShare(tc.authority, tc.inbox, previews, 1<<20).ServeHTTP(recorder, shareHTTP(t, tc.fileID, tc.body, tc.key))
		if recorder.Code != tc.status || tc.code != "" && !strings.Contains(recorder.Body.String(), `"`+tc.code+`"`) {
			t.Fatalf("%s: status %d body %s", tc.name, recorder.Code, recorder.Body)
		}
		if tc.createErr == nil && previews.createRequest.Target.Scheme != "" {
			t.Fatalf("%s: a refused share must not create a preview", tc.name)
		}
	}
}
