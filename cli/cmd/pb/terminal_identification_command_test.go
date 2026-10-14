package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionIdentificationListHumanAndJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "machine_ident", "alias": "hp", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"next_offset": nil}})
		case "/v1/machines/machine_ident/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "terminal_ident", "name": "backend-work", "title": "✳ Fixing auth", "foreground_process": "claude", "current_directory": "/projects/api", "started_in": "/home/user", "state": "detached"}, {"id": "terminal_shell", "name": "shell-work", "title": "", "foreground_process": "bash", "started_in": "/home/user", "state": "detached"}}, "pagination": map[string]any{"next_offset": nil}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	writeTestProfile(t, dir, configPath, server.URL)
	for _, mode := range []string{"", "--wide", "--json"} {
		var output bytes.Buffer
		args := []string{"--config", configPath, "session", "list", "hp"}
		if mode != "" {
			args = append(args, mode)
		}
		if code := run(context.Background(), args, &output, &output); code != 0 {
			t.Fatalf("%s exit%d: %s", mode, code, output.String())
		}
		for _, value := range []string{"backend-work", "✳ Fixing auth", "/projects/api", "shell-work", "bash"} {
			if !strings.Contains(output.String(), value) {
				t.Fatalf("%s omitted %q", mode, value)
			}
		}
		if mode == "--json" {
			if !strings.Contains(output.String(), `"current_directory":"/projects/api"`) || !strings.Contains(output.String(), `"started_in":"/home/user"`) {
				t.Fatal("JSON did not preserve distinct current/launch directory")
			}
		} else if !strings.Contains(output.String(), "Started in /home/user") || !strings.Contains(output.String(), "TITLE / PROCESS") {
			t.Fatal("human listing omitted useful metadata or mislabeled launch directory")
		}
	}
}
