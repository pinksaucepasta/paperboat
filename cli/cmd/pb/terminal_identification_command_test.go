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
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "machine_ident", "alias": "hp", "state": "ready", "online": true, "capabilities": map[string]any{"terminal_host": map[string]any{"configured": true, "observed": true}}}}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 1, "next_offset": nil}})
		case "/v1/machines/machine_ident/terminal-sessions":
			writeAPIData(t, w, map[string]any{"items": []map[string]any{{"id": "terminal_ident", "name": "backend-work", "title": "✳ Fixing auth", "foreground_process": "claude", "current_directory": "/projects/api", "started_in": "/home/user", "state": "detached"}, {"id": "terminal_shell", "name": "shell-work", "title": "", "foreground_process": "bash", "started_in": "/home/user", "state": "detached"}}, "pagination": map[string]any{"limit": echoedLimit(r), "offset": 0, "total": 2, "next_offset": nil}})
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

// X-25: without a machine, `pb session list` lists the account's sessions on every machine (the server catalog), so
// every computer of the account shows the same list; it once showed only the machine this client last connected to.
func TestSessionListWithoutAMachineListsEveryMachine(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/terminal-sessions":
			writeAPIData(t, w, map[string]any{"sessions": []map[string]any{
				{"id": "terminal_a", "name": "zoë build", "title": "vim", "state": "running", "target": map[string]any{"kind": "user_machine", "id": "machine_m", "name": "studio-mac"}},
				{"id": "terminal_b", "name": "logs", "foreground_process": "tail", "state": "detached", "target": map[string]any{"kind": "user_machine", "id": "machine_w", "name": "studio-win"}},
			}, "pagination": map[string]any{"limit": echoedLimit(r), "offset": 0, "total": 2, "next_offset": nil}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	writeTestProfile(t, dir, configPath, server.URL)
	for _, mode := range []string{"", "--wide", "--json"} {
		var output bytes.Buffer
		args := []string{"--config", configPath, "session", "list"}
		if mode != "" {
			args = append(args, mode)
		}
		if code := run(context.Background(), args, &output, &output); code != 0 {
			t.Fatalf("%s exit %d: %s", mode, code, output.String())
		}
		for _, value := range []string{"studio-mac", "studio-win", "zoë build", "logs"} {
			if !strings.Contains(output.String(), value) {
				t.Fatalf("%s omitted %q: %s", mode, value, output.String())
			}
		}
		if mode == "" && !strings.Contains(output.String(), "MACHINE") {
			t.Fatalf("the list does not say which machine each session is on: %s", output.String())
		}
		if mode == "--wide" && !strings.Contains(output.String(), "terminal_b") {
			t.Fatalf("--wide omitted the session ID: %s", output.String())
		}
	}
}
