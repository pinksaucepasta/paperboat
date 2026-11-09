package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
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
	if err := SaveReplacement("machine_abc", false, now); err != nil {
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
	if SaveReplacement("bad id/with slash", false, now) == nil {
		t.Fatal("an invalid id was saved")
	}
}

// A reset after a failed pairing must not make the note forget the record an earlier reset left
// behind: every record stays until it is replaced, and an unfinished one stays marked.
func TestReplacementNoteKeepsEveryRecordTheComputerLeftBehind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", shorttemp.Dir(t))
	t.Setenv("HOME", shorttemp.Dir(t))
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, step := range []Replacement{{MachineID: "machine_old"}, {MachineID: "machine_failed", Unfinished: true}, {MachineID: "machine_failed"}} {
		if err := SaveReplacement(step.MachineID, step.Unfinished, now); err != nil {
			t.Fatal(err)
		}
	}
	left := LoadReplacements(now)
	if len(left) != 2 || left[0] != (Replacement{MachineID: "machine_old"}) || left[1] != (Replacement{MachineID: "machine_failed", Unfinished: true}) {
		t.Fatalf("note=%+v, want the old record and the unfinished one (still marked)", left)
	}
	if got := LoadReplacement(now); got != "machine_failed" {
		t.Fatalf("the pairing names %q, want the record this computer was last", got)
	}
	if err := ForgetReplacement("machine_failed", now); err != nil {
		t.Fatal(err)
	}
	if left = LoadReplacements(now.Add(23 * time.Hour)); len(left) != 1 || left[0].MachineID != "machine_old" {
		t.Fatalf("after forgetting the replaced record note=%+v", left)
	}
	if err := ForgetReplacement("machine_old", now); err != nil {
		t.Fatal(err)
	}
	if left = LoadReplacements(now); len(left) != 0 {
		t.Fatalf("an empty note still loads %+v", left)
	}
	// Bounded: the oldest record (it holds the name and address) and the newest ones are kept.
	for index := 0; index < maxReplacements+5; index++ {
		if err := SaveReplacement(fmt.Sprintf("machine_%d", index), false, now); err != nil {
			t.Fatal(err)
		}
	}
	left = LoadReplacements(now)
	if len(left) != maxReplacements || left[0].MachineID != "machine_0" || left[len(left)-1].MachineID != fmt.Sprintf("machine_%d", maxReplacements+4) {
		t.Fatalf("bounded note has %d records, first %q last %q", len(left), left[0].MachineID, left[len(left)-1].MachineID)
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
