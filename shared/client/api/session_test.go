package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A CLI on a paired machine names it at token sign-in (F18-06); an SDK endpoint names none.
func TestStartAPITokenSessionNamesThePairedMachineOnlyWhenGiven(t *testing.T) {
	var got []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		_, _ = w.Write([]byte(`{"data":{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":60}}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Options: CallOptions{HTTPClient: server.Client()}}
	for _, machine := range []string{"machine_paired", ""} {
		if _, err := client.StartAPITokenSession(context.Background(), "token", "label", "server", "linux", machine); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0]["enrolled_machine_id"] != "machine_paired" {
		t.Fatalf("paired sign-in body = %v", got)
	}
	if _, present := got[1]["enrolled_machine_id"]; present {
		t.Fatalf("SDK sign-in named a machine: %v", got[1])
	}
}
