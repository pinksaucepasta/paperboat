package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestConfirmationCodesAreIndependentAndServerBound(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	newCommand := func(server, token string, output *bytes.Buffer) *cobra.Command {
		command := &cobra.Command{Use: "delete"}
		command.Flags().String("config", "", "")
		if err := command.Flags().Set("config", configPath); err != nil {
			t.Fatal(err)
		}
		command.Flags().String("server", "", "")
		command.Flags().String("confirm", "", "")
		if err := command.Flags().Set("server", server); err != nil {
			t.Fatal(err)
		}
		if token != "" {
			if err := command.Flags().Set("confirm", token); err != nil {
				t.Fatal(err)
			}
		}
		command.SetOut(output)
		return command
	}
	var first, second bytes.Buffer
	if err := confirmMutation(newCommand("https://one.example.test", "", &first), "first", "Delete first?"); err == nil || err.(exitCodeError).code != 2 {
		t.Fatalf("first preview: %v", err)
	}
	if err := confirmMutation(newCommand("https://one.example.test", "", &second), "second", "Delete second?"); err == nil || err.(exitCodeError).code != 2 {
		t.Fatalf("second preview: %v", err)
	}
	firstToken, secondToken := previewConfirmationCode(t, first.String()), previewConfirmationCode(t, second.String())
	if !strings.Contains(first.String(), "--server 'https://one.example.test'") {
		t.Fatalf("preview command lost server override: %q", first.String())
	}
	if err := confirmMutation(newCommand("https://two.example.test", firstToken, &bytes.Buffer{}), "first", "Delete first?"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("cross-server code accepted: %v", err)
	}
	if err := confirmMutation(newCommand("https://one.example.test", secondToken, &bytes.Buffer{}), "second", "Delete second?"); err != nil {
		t.Fatalf("second preview was invalidated: %v", err)
	}
}

func TestSessionDeleteAllTokenScopeExpiryAndReplay(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	ids := []string{"ses_1"}
	deletes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "um_1", "alias": "demo", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"next_offset": nil}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines/um_1/terminal-sessions":
			items := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				items = append(items, map[string]any{"id": id, "name": id, "state": "closed"})
			}
			writeAPIData(t, w, map[string]any{"items": items, "pagination": map[string]any{"next_offset": nil}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/machines/um_1/terminal-sessions/"):
			deletes++
			writeAPIData(t, w, map[string]any{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	writeTestProfile(t, dir, configPath, srv.URL)
	runDelete := func(extra ...string) (int, string) {
		t.Helper()
		args := append([]string{"--config", configPath, "session", "delete", "demo", "--all"}, extra...)
		var output bytes.Buffer
		code := run(context.Background(), args, &output, &output)
		return code, output.String()
	}
	code, output := runDelete("--yes")
	if code != 2 || deletes != 0 || !strings.Contains(output, "unknown flag") {
		t.Fatalf("--yes bypassed preview: code=%d deletes=%d output=%q", code, deletes, output)
	}
	code, output = runDelete()
	if code != 2 {
		t.Fatalf("preview code=%d output=%q", code, output)
	}
	token := previewConfirmationCode(t, output)
	wrong := "AAAAAA"
	if token == wrong {
		wrong = "BBBBBB"
	}
	if code, output = runDelete("--confirm", wrong); code == 0 || deletes != 0 || !strings.Contains(output, "missing or already used") {
		t.Fatalf("wrong token accepted: code=%d deletes=%d output=%q", code, deletes, output)
	}
	ids = append(ids, "ses_2")
	if code, output = runDelete("--confirm", token); code == 0 || deletes != 0 || !strings.Contains(output, "does not match") {
		t.Fatalf("changed scope accepted: code=%d deletes=%d output=%q", code, deletes, output)
	}
	code, output = runDelete()
	if code != 2 {
		t.Fatalf("second preview: code=%d output=%q", code, output)
	}
	token = previewConfirmationCode(t, output)
	path := configPath + ".confirmation.json." + token
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var challenge confirmationChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatal(err)
	}
	challenge.ExpiresAt = time.Now().Add(-time.Second)
	data, err = json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output = runDelete("--confirm", token); code == 0 || deletes != 0 || !strings.Contains(output, "expired") {
		t.Fatalf("expired token accepted: code=%d deletes=%d output=%q", code, deletes, output)
	}
	code, output = runDelete()
	if code != 2 {
		t.Fatalf("third preview: code=%d output=%q", code, output)
	}
	token = previewConfirmationCode(t, output)
	if code, output = runDelete("--confirm", token); code != 0 || deletes != 2 {
		t.Fatalf("confirmation failed: code=%d deletes=%d output=%q", code, deletes, output)
	}
	if code, output = runDelete("--confirm", token); code == 0 || deletes != 2 || !strings.Contains(output, "already used") {
		t.Fatalf("token replay accepted: code=%d deletes=%d output=%q", code, deletes, output)
	}
}
