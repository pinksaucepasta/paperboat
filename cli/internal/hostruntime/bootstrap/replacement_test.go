package bootstrap

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReplacementNoteSurvivesTheWipeAndExpires(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", shorttemp.Dir(t))
	t.Setenv("HOME", shorttemp.Dir(t))
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if got := LoadReplacement(now); got != "" {
		t.Fatalf("no note yet, got %q", got)
	}
	if err := SaveReplacement("machine_abc", now); err != nil {
		t.Fatal(err)
	}
	if got := LoadReplacement(now.Add(time.Hour)); got != "machine_abc" {
		t.Fatalf("note=%q", got)
	}
	if got := LoadReplacement(now.Add(25 * time.Hour)); got != "" {
		t.Fatalf("an old note was honoured: %q", got)
	}
	if err := ClearReplacement(); err != nil {
		t.Fatal(err)
	}
	if got := LoadReplacement(now); got != "" {
		t.Fatalf("cleared note still loaded: %q", got)
	}
	if SaveReplacement("bad id/with slash", now) == nil {
		t.Fatal("an invalid id was saved")
	}
}

func TestCreatePairingCarriesTheMachineItReplaces(t *testing.T) {
	var sent map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"pairing_1","user_code":"ABCD-EFGH","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}}`))
	}))
	defer server.Close()
	config := Config{ServerURL: server.URL, EnrollmentToken: "ABCDEFGHIJKLMNOPQRSTUVWXYZ", Alias: "laptop", WorkspaceRoot: shorttemp.Dir(t), Verifier: "0123456789012345678901234567890123456789", PublicIdentityKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", HTTP: server.Client()}
	if _, err := CreatePairing(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if _, present := sent["replaces_machine_id"]; present {
		t.Fatal("replaces_machine_id sent without a replacement")
	}
	config.ReplacesMachineID = "machine_old"
	if _, err := CreatePairing(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if sent["replaces_machine_id"] != "machine_old" {
		t.Fatalf("pairing body %v", sent)
	}
}
