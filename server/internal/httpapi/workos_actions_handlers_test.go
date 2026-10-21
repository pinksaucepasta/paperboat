package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type aliasCheckerFake struct {
	canonical string
	exists    bool
	err       error
	asked     string
}

func (f *aliasCheckerFake) ExistingAccountForAlias(_ context.Context, submitted string) (string, bool, error) {
	f.asked = submitted
	return f.canonical, f.exists, f.err
}

const workOSActionTestSecret = "actions-secret-for-tests"

func workOSActionRequest(t *testing.T, body string, at time.Time, secret string) *http.Request {
	t.Helper()
	stamp := strconv.FormatInt(at.UnixMilli(), 10)
	signature := ""
	for _, b := range workOSActionMAC(secret, stamp, []byte(body)) {
		signature += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15])
	}
	r := httptest.NewRequest("POST", "/v1/webhooks/workos/registration", strings.NewReader(body))
	r.Header.Set("WorkOS-Signature", "t="+stamp+", v1="+signature)
	return r
}

const registrationBody = `{"id":"action_1","object":"user_registration_action_context","user_data":{"object":"user_data","email":"Pprbtdev+1234@gmail.com","first_name":"A"}}`

func TestWorkOSRegistrationActionDeniesAnAliasOfAnExistingAccount(t *testing.T) {
	now := time.Now()
	checker := &aliasCheckerFake{canonical: "pprbtdev@gmail.com", exists: true}
	handler := workOSRegistrationAction(checker, workOSActionTestSecret, func() time.Time { return now })
	w := httptest.NewRecorder()
	handler(w, workOSActionRequest(t, registrationBody, now, workOSActionTestSecret))
	if w.Code != 200 || checker.asked != "Pprbtdev+1234@gmail.com" {
		t.Fatalf("status=%d asked=%q body=%s", w.Code, checker.asked, w.Body)
	}
	var response struct {
		Object  string          `json:"object"`
		Payload json.RawMessage `json:"payload"`
		Sig     string          `json:"signature"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Object != "user_registration_action_response" {
		t.Fatalf("response=%s err=%v", w.Body, err)
	}
	var payload struct {
		Timestamp int64  `json:"timestamp"`
		Verdict   string `json:"verdict"`
		Message   string `json:"error_message"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil || payload.Verdict != "Deny" || payload.Message != "An account for pprbtdev@gmail.com already exists. Sign in with that address." {
		t.Fatalf("payload=%s err=%v", response.Payload, err)
	}
	want := workOSActionMAC(workOSActionTestSecret, strconv.FormatInt(payload.Timestamp, 10), response.Payload)
	got := ""
	for _, b := range want {
		got += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15])
	}
	if response.Sig != got {
		t.Fatal("response signature does not cover timestamp.payload")
	}
}

func TestWorkOSRegistrationActionAllowsAnythingElse(t *testing.T) {
	now := time.Now()
	handler := workOSRegistrationAction(&aliasCheckerFake{canonical: "new@example.test"}, workOSActionTestSecret, func() time.Time { return now })
	w := httptest.NewRecorder()
	handler(w, workOSActionRequest(t, registrationBody, now, workOSActionTestSecret))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `\"verdict\":\"Allow\"`) && !strings.Contains(w.Body.String(), `"verdict":"Allow"`) || strings.Contains(w.Body.String(), "error_message") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}

func TestWorkOSRegistrationActionRefusesUnsignedStaleAndMalformedRequests(t *testing.T) {
	now := time.Now()
	checker := &aliasCheckerFake{canonical: "pprbtdev@gmail.com", exists: true}
	handler := workOSRegistrationAction(checker, workOSActionTestSecret, func() time.Time { return now })
	for name, r := range map[string]*http.Request{
		"wrong secret": workOSActionRequest(t, registrationBody, now, "another-secret"),
		"stale":        workOSActionRequest(t, registrationBody, now.Add(-10*time.Minute), workOSActionTestSecret),
		"no header":    httptest.NewRequest("POST", "/v1/webhooks/workos/registration", strings.NewReader(registrationBody)),
	} {
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 401 || checker.asked != "" {
			t.Fatalf("%s: status=%d asked=%q", name, w.Code, checker.asked)
		}
	}
	w := httptest.NewRecorder()
	handler(w, workOSActionRequest(t, `{"object":"authentication_action_context","user_data":{"email":"a@b.test"}}`, now, workOSActionTestSecret))
	if w.Code != 400 {
		t.Fatalf("other action type: status=%d", w.Code)
	}
}

func TestWorkOSRegistrationActionNeverDeniesOnAnInternalError(t *testing.T) {
	now := time.Now()
	handler := workOSRegistrationAction(&aliasCheckerFake{err: context.DeadlineExceeded}, workOSActionTestSecret, func() time.Time { return now })
	w := httptest.NewRecorder()
	handler(w, workOSActionRequest(t, registrationBody, now, workOSActionTestSecret))
	if w.Code != 503 || strings.Contains(w.Body.String(), "Deny") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
