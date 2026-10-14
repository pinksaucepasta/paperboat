package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpdatePolicyDeadlineAndResponseValidation(t *testing.T) {
	deadline := time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
	p := UpdatePolicy{Schema: "paperboat.client-update-policy/v1", Revision: 2, MinimumVersion: "2026.10.08.2", Reason: "Protocol update", EnforceAt: &deadline}
	if p.Required("2026.10.08.1", deadline.Add(-time.Nanosecond)) || !p.Required("2026.10.08.1", deadline) || p.Required("2026.10.08.2", deadline) {
		t.Fatal("deadline/floor enforcement incorrect")
	}
	if !p.Required("unknown", deadline) {
		t.Fatal("unidentified build accepted after deadline")
	}
	for _, body := range []string{
		`{"data":{"schema":"paperboat.client-update-policy/v1","revision":2,"minimum_version":"2026.10.08.2","reason":"Protocol update"}}`,
		`{"data":{"schema":"paperboat.client-update-policy/v1","revision":2,"minimum_version":"2026.10.08.2","reason":"\u001b[31m"}}`,
		`{"data":{"schema":"future","revision":2}}`,
		`{"data":{"schema":"paperboat.client-update-policy/v1","revision":2,"minimum_version":"bad"}}`,
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.Header.Get("Authorization") != "" {
				t.Error("policy request must be public GET")
			}
			fmt.Fprint(w, body)
		}))
		got, err := FetchUpdatePolicy(context.Background(), server.URL, server.Client())
		server.Close()
		wantValid := body == `{"data":{"schema":"paperboat.client-update-policy/v1","revision":2,"minimum_version":"2026.10.08.2","reason":"Protocol update"}}`
		if (err == nil) != wantValid {
			t.Fatalf("response valid=%v got=%+v err=%v", wantValid, got, err)
		}
	}
}
