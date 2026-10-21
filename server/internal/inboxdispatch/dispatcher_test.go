package inboxdispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

const testIssuer = "https://api.example.test"

type fakeMachine struct {
	t       *testing.T
	signer  *mint.Provider
	handler func(http.ResponseWriter, *http.Request, mint.CredentialClaims)
	mu      sync.Mutex // the dispatcher may issue concurrent requests; these two fields are shared
	claims  mint.CredentialClaims
	seen    int
}

func (m *fakeMachine) lastClaims() mint.CredentialClaims {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claims
}

func (m *fakeMachine) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen
}

func (m *fakeMachine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.seen++
	m.mu.Unlock()
	class := map[string]string{http.MethodGet: "inbox_read", http.MethodDelete: "inbox_delete"}[r.Method]
	if r.URL.Path == "/v1/inbox-files" {
		class = "inbox_list"
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := m.signer.VerifyCredential(token, testIssuer, class, time.Now().UTC())
	if err != nil {
		m.t.Errorf("credential rejected: %v", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	m.mu.Lock()
	m.claims = claims
	m.mu.Unlock()
	m.handler(w, r, claims)
}

func dispatcherFor(t *testing.T, machine *fakeMachine) (*Dispatcher, *httptest.Server) {
	t.Helper()
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	machine.t, machine.signer = t, signer
	server := httptest.NewServer(machine)
	t.Cleanup(server.Close)
	dispatcher := &Dispatcher{signer: signer, issuer: testIssuer, client: server.Client(), headerTimeout: 10 * time.Second, maxFileBytes: 1 << 20, now: func() time.Time { return time.Now().UTC() },
		newJTI: func() (string, error) { return "jti_" + time.Now().Format("150405.000000000"), nil }}
	dispatcher.client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect rejected") }
	dispatcher.resolve = func(context.Context, Request) (route, error) {
		return route{EnvironmentID: "env_1", OwnerID: "owner_1", BaseURL: server.URL}, nil
	}
	return dispatcher, server
}

func request() Request {
	return Request{ActorID: "actor_1", MachineID: "machine_1", FileID: "0123456789abcdef", OperationID: "operation_inbox_1", RequestID: "request_1", CorrelationID: "correlation_1", IdempotencyKey: "delete_key_1"}
}

func TestOpenStreamsTheFileWithABoundSingleUseCredential(t *testing.T) {
	machine := &fakeMachine{handler: func(w http.ResponseWriter, r *http.Request, claims mint.CredentialClaims) {
		if r.URL.Path != "/v1/inbox-files/0123456789abcdef" || r.Header.Get("X-Paperboat-Operation-Id") != "operation_inbox_1" || claims.ResourceID != "0123456789abcdef" {
			t.Errorf("path=%s claims=%+v", r.URL.Path, claims)
		}
		w.Header().Set("Content-Length", "11")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''r%C3%A9sum%C3%A9%20%281%29.txt")
		_, _ = w.Write([]byte("hello inbox"))
	}}
	dispatcher, _ := dispatcherFor(t, machine)
	file, err := dispatcher.Open(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Body.Close()
	body, err := io.ReadAll(file.Body)
	if err != nil || string(body) != "hello inbox" || file.Size != 11 || file.Name != "résumé (1).txt" {
		t.Fatalf("body=%q size=%d name=%q err=%v", body, file.Size, file.Name, err)
	}
	if machine.lastClaims().MachineID != "machine_1" || machine.lastClaims().UserID != "actor_1" || machine.lastClaims().OperationID != "operation_inbox_1" || machine.lastClaims().AccountID != "owner_1" || machine.lastClaims().EnvironmentID != "env_1" || machine.lastClaims().IdempotencyKey != "" {
		t.Fatalf("claims = %+v", machine.lastClaims())
	}
}

func TestDeleteCarriesTheIdempotencyKeyAndMapsAnswers(t *testing.T) {
	answer := http.StatusNoContent
	machine := &fakeMachine{handler: func(w http.ResponseWriter, r *http.Request, claims mint.CredentialClaims) {
		if claims.IdempotencyKey != "delete_key_1" {
			t.Errorf("idempotency key = %q", claims.IdempotencyKey)
		}
		w.WriteHeader(answer)
	}}
	dispatcher, _ := dispatcherFor(t, machine)
	for status, want := range map[int]error{
		http.StatusNoContent:           nil,
		http.StatusNotFound:            ErrFileNotFound,
		http.StatusConflict:            ErrCapabilityDisabled,
		http.StatusForbidden:           ErrMachineUnreachable,
		http.StatusInternalServerError: ErrMachineUnreachable,
	} {
		answer = status
		err := dispatcher.Delete(t.Context(), request())
		if want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Errorf("status %d: err=%v want %v", status, err, want)
		}
	}
}

func TestOpenRefusesTooLargeShortAndSizelessAnswers(t *testing.T) {
	cases := map[string]struct {
		handler func(http.ResponseWriter)
		want    error
	}{
		"too large": {func(w http.ResponseWriter) { w.Header().Set("Content-Length", "2097152"); _, _ = w.Write([]byte("x")) }, ErrFileTooLarge},
		"not found": {func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, ErrFileNotFound},
		"no size": {func(w http.ResponseWriter) {
			w.Header().Set("Transfer-Encoding", "chunked")
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
		}, ErrMachineUnreachable},
	}
	for name, tc := range cases {
		machine := &fakeMachine{handler: func(w http.ResponseWriter, _ *http.Request, _ mint.CredentialClaims) { tc.handler(w) }}
		dispatcher, _ := dispatcherFor(t, machine)
		if _, err := dispatcher.Open(t.Context(), request()); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	short := &fakeMachine{handler: func(w http.ResponseWriter, _ *http.Request, _ mint.CredentialClaims) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("only4"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}}
	dispatcher, _ := dispatcherFor(t, short)
	file, err := dispatcher.Open(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Body.Close()
	if _, err := io.ReadAll(file.Body); err == nil {
		t.Fatal("a body shorter than announced looked complete")
	}
}

func TestMachineThatAnswersTooSlowlyIsUnreachableAndRedirectsAreNotFollowed(t *testing.T) {
	slow := &fakeMachine{handler: func(w http.ResponseWriter, _ *http.Request, _ mint.CredentialClaims) {
		time.Sleep(900 * time.Millisecond)
		w.Header().Set("Content-Length", "0")
	}}
	dispatcher, _ := dispatcherFor(t, slow)
	dispatcher.headerTimeout = 300 * time.Millisecond // the machine answers after 900 ms
	if _, err := dispatcher.Open(t.Context(), request()); !errors.Is(err, ErrMachineUnreachable) {
		t.Fatalf("slow machine: %v", err)
	}
	redirecting := &fakeMachine{handler: func(w http.ResponseWriter, r *http.Request, _ mint.CredentialClaims) {
		http.Redirect(w, r, "https://elsewhere.example.test/steal", http.StatusFound)
	}}
	dispatcher, _ = dispatcherFor(t, redirecting)
	if _, err := dispatcher.Open(t.Context(), request()); !errors.Is(err, ErrMachineUnreachable) {
		t.Fatalf("redirect: %v", err)
	}
}

func TestOfflineMachineAndMalformedRequestsDoNotReachTheNetwork(t *testing.T) {
	machine := &fakeMachine{handler: func(w http.ResponseWriter, _ *http.Request, _ mint.CredentialClaims) {
		w.WriteHeader(http.StatusNoContent)
	}}
	dispatcher, _ := dispatcherFor(t, machine)
	dispatcher.resolve = func(context.Context, Request) (route, error) { return route{}, ErrMachineOffline }
	if err := dispatcher.Delete(t.Context(), request()); !errors.Is(err, ErrMachineOffline) {
		t.Fatalf("offline: %v", err)
	}
	empty := request()
	empty.FileID = ""
	if _, err := dispatcher.Open(t.Context(), empty); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("empty file id: %v", err)
	}
	if machine.calls() != 0 {
		t.Fatalf("machine was called %d times", machine.calls())
	}
	for _, base := range []string{"ftp://x", "https://user:pw@host", "https://host?x=1", "https://host/v1/runtime/a/b", "https://host/other"} {
		if _, err := inboxEndpoint(base, "0123456789abcdef", false); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	if got, err := inboxEndpoint("https://edge.example.test/v1/runtime/host-1", "0123456789abcdef", false); err != nil || got != "https://edge.example.test/v1/runtime/host-1/v1/inbox-files/0123456789abcdef" {
		t.Fatalf("endpoint = %q %v", got, err)
	}
	if _, err := inboxEndpoint("https://host", "../x", false); err == nil {
		t.Fatal("file id with a path accepted")
	}
}

func TestListAsksTheMachineLiveWithAListCredential(t *testing.T) {
	machine := &fakeMachine{handler: func(w http.ResponseWriter, r *http.Request, claims mint.CredentialClaims) {
		if r.URL.Path != "/v1/inbox-files" || claims.ResourceKind != mint.InboxListResourceKind || claims.ResourceID != "machine_1" || claims.CredentialClass != "inbox_list" {
			t.Errorf("path=%s claims=%+v", r.URL.Path, claims)
		}
		_, _ = w.Write([]byte(`{"total":3,"items":[{"id":"0123456789abcdef","name":"a.txt","size":3,"received_at":"2026-10-18T09:00:00Z"}]}`))
	}}
	dispatcher, _ := dispatcherFor(t, machine)
	// A file id on a list request is ignored: a listing is about the machine, not a file.
	listRequest := request()
	listRequest.FileID = "ignored"
	got, err := dispatcher.List(t.Context(), listRequest)
	if err != nil || got.Total != 3 || len(got.Items) != 1 || got.Items[0].Name != "a.txt" || got.Items[0].Size != 3 {
		t.Fatalf("listing = %+v err=%v", got, err)
	}
}

func TestListRefusesInvalidListingsAndMapsAnswers(t *testing.T) {
	body, status := "", http.StatusOK
	machine := &fakeMachine{handler: func(w http.ResponseWriter, _ *http.Request, _ mint.CredentialClaims) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}}
	dispatcher, _ := dispatcherFor(t, machine)
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"path in name":   {200, `{"total":1,"items":[{"id":"0123456789abcdef","name":"../x","size":1,"received_at":"2026-10-18T09:00:00Z"}]}`, ErrMachineUnreachable},
		"total below":    {200, `{"total":0,"items":[{"id":"0123456789abcdef","name":"a","size":1,"received_at":"2026-10-18T09:00:00Z"}]}`, ErrMachineUnreachable},
		"short id":       {200, `{"total":1,"items":[{"id":"abc","name":"a","size":1,"received_at":"2026-10-18T09:00:00Z"}]}`, ErrMachineUnreachable},
		"unknown field":  {200, `{"total":0,"items":[],"extra":1}`, ErrMachineUnreachable},
		"not json":       {200, `<html>`, ErrMachineUnreachable},
		"capability off": {409, ``, ErrCapabilityDisabled},
		"older daemon":   {404, ``, ErrMachineOutdated},
		"machine error":  {500, ``, ErrMachineUnreachable},
	} {
		status, body = tc.status, tc.body
		if _, err := dispatcher.List(t.Context(), request()); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	status, body = 200, `{"total":0,"items":null}`
	if got, err := dispatcher.List(t.Context(), request()); err != nil || got.Items == nil || len(got.Items) != 0 {
		t.Errorf("empty listing = %+v %v", got, err)
	}
	dispatcher.resolve = func(context.Context, Request) (route, error) { return route{}, ErrMachineOffline }
	if _, err := dispatcher.List(t.Context(), request()); !errors.Is(err, ErrMachineOffline) {
		t.Errorf("offline: %v", err)
	}
}
