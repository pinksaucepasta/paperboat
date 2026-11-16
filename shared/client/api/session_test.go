package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
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

// A recipe session sends the token id and its derived auth, never the secret, without a bearer (F26).
func TestStartRecipeSessionSendsOnlyTheIDAndAuth(t *testing.T) {
	token, err := recipe.NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		TokenID string `json:"token_id"`
		Auth    []byte `json:"auth"`
	}
	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/recipe-sessions" || r.Header.Get("Authorization") != "" {
			t.Errorf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"data":{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":60,"recipe":{"id":"rcp_1","name":"ci","revision":2,"workspace":"personal","recipe":{"tags":["ci"]}}}}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Options: CallOptions{HTTPClient: server.Client()}}
	out, err := client.StartRecipeSession(context.Background(), token, "label", "server", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if body.TokenID != token.ID || !bytes.Equal(body.Auth, token.Auth()) || bytes.Contains(raw, []byte(token.String()[len(recipe.TokenPrefix)+11:])) {
		t.Fatalf("body = %s", raw)
	}
	if out.AccessToken != "a" || out.Recipe.Name != "ci" || out.Recipe.Revision != 2 || string(out.Recipe.Recipe) != `{"tags":["ci"]}` {
		t.Fatalf("out = %+v", out)
	}
}
