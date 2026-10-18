package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/inboxdispatch"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type fakeInboxAuthority struct{ err error }

func (f fakeInboxAuthority) CanManageInbox(context.Context, string, string) error { return f.err }

type fakeInboxAPI struct {
	listErr            error
	listing            inboxdispatch.Listing
	listed             []inboxdispatch.Request
	openErr, deleteErr error
	file               inboxdispatch.File
	opened, deleted    []inboxdispatch.Request
}

func (f *fakeInboxAPI) List(_ context.Context, request inboxdispatch.Request) (inboxdispatch.Listing, error) {
	f.listed = append(f.listed, request)
	return f.listing, f.listErr
}

func (f *fakeInboxAPI) Open(_ context.Context, request inboxdispatch.Request) (inboxdispatch.File, error) {
	f.opened = append(f.opened, request)
	return f.file, f.openErr
}

func (f *fakeInboxAPI) Delete(_ context.Context, request inboxdispatch.Request) error {
	f.deleted = append(f.deleted, request)
	return f.deleteErr
}

func inboxHTTP(method, fileID string, signedIn bool, headers map[string]string) *http.Request {
	request := httptest.NewRequest(method, "/v1/machines/machine_1/inbox/"+fileID, nil)
	request.SetPathValue("machine_id", "machine_1")
	request.SetPathValue("file_id", fileID)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if signedIn {
		request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "user_1"}}))
	}
	return request
}

const validInboxFile = "0123456789abcdef"

func TestInboxListIsServedLiveFromTheMachine(t *testing.T) {
	api := &fakeInboxAPI{listing: inboxdispatch.Listing{Total: 1, Items: []inboxdispatch.Item{{ID: validInboxFile, Name: "a.txt", Size: 3}}}}
	recorder := httptest.NewRecorder()
	userMachineInboxList(fakeInboxAuthority{}, api).ServeHTTP(recorder, inboxHTTP(http.MethodGet, "", true, nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"a.txt"`) || !strings.Contains(recorder.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(api.listed) != 1 || api.listed[0].ActorID != "user_1" || api.listed[0].MachineID != "machine_1" || api.listed[0].FileID != "" {
		t.Fatalf("request = %+v", api.listed)
	}
	for name, tc := range map[string]struct {
		authority inboxAuthority
		err       error
		signedIn  bool
		status    int
		code      string
	}{
		"offline machine": {fakeInboxAuthority{}, inboxdispatch.ErrMachineOffline, true, http.StatusConflict, "machine_offline"},
		"unreachable":     {fakeInboxAuthority{}, inboxdispatch.ErrMachineUnreachable, true, http.StatusServiceUnavailable, "machine_unreachable"},
		"stranger":        {fakeInboxAuthority{err: usermachines.ErrNotFound}, nil, true, http.StatusNotFound, "user_machine_not_found"},
		"anonymous":       {fakeInboxAuthority{}, nil, false, http.StatusUnauthorized, "unauthenticated"},
	} {
		api := &fakeInboxAPI{listErr: tc.err}
		recorder := httptest.NewRecorder()
		userMachineInboxList(tc.authority, api).ServeHTTP(recorder, inboxHTTP(http.MethodGet, "", tc.signedIn, nil))
		if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s", name, recorder.Code, recorder.Body.String())
		}
		if tc.status == http.StatusNotFound || tc.status == http.StatusUnauthorized {
			if len(api.listed) != 0 {
				t.Errorf("%s: machine reached for a refused request", name)
			}
		}
	}
}

func TestInboxContentStreamsTheFileAsAnAttachment(t *testing.T) {
	api := &fakeInboxAPI{file: inboxdispatch.File{Body: io.NopCloser(strings.NewReader("hello inbox")), Size: 11, Name: "report final.pdf"}}
	recorder := httptest.NewRecorder()
	userMachineInboxContent(fakeInboxAuthority{}, api).ServeHTTP(recorder, inboxHTTP(http.MethodGet, validInboxFile, true, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "hello inbox" || recorder.Header().Get("Content-Length") != "11" ||
		recorder.Header().Get("Content-Disposition") != "attachment; filename*=UTF-8''report%20final.pdf" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" ||
		recorder.Header().Get("Content-Type") != "application/octet-stream" || !strings.Contains(recorder.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if len(api.opened) != 1 || api.opened[0].ActorID != "user_1" || api.opened[0].MachineID != "machine_1" || api.opened[0].FileID != validInboxFile || !strings.HasPrefix(api.opened[0].OperationID, "inbox-") {
		t.Fatalf("request = %+v", api.opened)
	}
}

func TestInboxHandlersMapEveryFailureToItsStableCode(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{inboxdispatch.ErrFileNotFound, http.StatusNotFound, "inbox_file_not_found"},
		{inboxdispatch.ErrMachineOffline, http.StatusConflict, "machine_offline"},
		{inboxdispatch.ErrCapabilityDisabled, http.StatusConflict, "inbox_unavailable"},
		{inboxdispatch.ErrMachineOutdated, http.StatusConflict, "machine_update_required"},
		{inboxdispatch.ErrFileTooLarge, http.StatusRequestEntityTooLarge, "file_too_large"},
		{errors.Join(inboxdispatch.ErrMachineUnreachable, errors.New("dial tcp")), http.StatusServiceUnavailable, "machine_unreachable"},
		{errors.New("database exploded"), http.StatusInternalServerError, "internal_error"},
	} {
		read := httptest.NewRecorder()
		userMachineInboxContent(fakeInboxAuthority{}, &fakeInboxAPI{openErr: tc.err}).ServeHTTP(read, inboxHTTP(http.MethodGet, validInboxFile, true, nil))
		remove := httptest.NewRecorder()
		userMachineInboxDelete(fakeInboxAuthority{}, &fakeInboxAPI{deleteErr: tc.err}).ServeHTTP(remove, inboxHTTP(http.MethodDelete, validInboxFile, true, map[string]string{"Idempotency-Key": "key_1"}))
		for name, recorder := range map[string]*httptest.ResponseRecorder{"read": read, "delete": remove} {
			if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), `"`+tc.code+`"`) {
				t.Errorf("%s %v: status=%d body=%s", name, tc.err, recorder.Code, recorder.Body.String())
			}
		}
	}
}

func TestInboxDeleteNeedsAnIdempotencyKeyAndBindsItToTheMachineCall(t *testing.T) {
	api := &fakeInboxAPI{}
	handler := userMachineInboxDelete(fakeInboxAuthority{}, api)
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, inboxHTTP(http.MethodDelete, validInboxFile, true, nil))
	if missing.Code != http.StatusBadRequest || len(api.deleted) != 0 {
		t.Fatalf("missing key: %d deleted=%d", missing.Code, len(api.deleted))
	}
	done := httptest.NewRecorder()
	handler.ServeHTTP(done, inboxHTTP(http.MethodDelete, validInboxFile, true, map[string]string{"Idempotency-Key": "delete_key_1"}))
	if done.Code != http.StatusNoContent || len(api.deleted) != 1 || api.deleted[0].IdempotencyKey != "delete_key_1" {
		t.Fatalf("delete: %d %+v", done.Code, api.deleted)
	}
}

func TestInboxHandlersRefuseStrangersMalformedIDsAndAnonymousCallers(t *testing.T) {
	api := &fakeInboxAPI{}
	for name, tc := range map[string]struct {
		authority inboxAuthority
		fileID    string
		signedIn  bool
		status    int
	}{
		"stranger":     {fakeInboxAuthority{err: usermachines.ErrNotFound}, validInboxFile, true, http.StatusNotFound},
		"bad id":       {fakeInboxAuthority{}, "../etc/passwd", true, http.StatusNotFound},
		"upper id":     {fakeInboxAuthority{}, "0123456789ABCDEF", true, http.StatusNotFound},
		"anonymous":    {fakeInboxAuthority{}, validInboxFile, false, http.StatusUnauthorized},
		"authority db": {fakeInboxAuthority{err: errors.New("db down")}, validInboxFile, true, http.StatusInternalServerError},
	} {
		read := httptest.NewRecorder()
		userMachineInboxContent(tc.authority, api).ServeHTTP(read, inboxHTTP(http.MethodGet, tc.fileID, tc.signedIn, nil))
		remove := httptest.NewRecorder()
		userMachineInboxDelete(tc.authority, api).ServeHTTP(remove, inboxHTTP(http.MethodDelete, tc.fileID, tc.signedIn, map[string]string{"Idempotency-Key": "k"}))
		if read.Code != tc.status || remove.Code != tc.status {
			t.Errorf("%s: read=%d delete=%d want %d", name, read.Code, remove.Code, tc.status)
		}
	}
	if len(api.opened)+len(api.deleted) != 0 {
		t.Fatalf("machine reached for a refused request: %d %d", len(api.opened), len(api.deleted))
	}
}
