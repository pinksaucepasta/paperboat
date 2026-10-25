package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// tagServer answers the machine list, the policy version and the machine tag PUT; the PUT is
// refused once with 412 to check the access-change review.
type tagServer struct {
	mu       sync.Mutex
	puts     []map[string]any
	refuse   bool
	machines []api.UserMachine
}

func (s *tagServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/machines":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			writeAPIData(t, w, api.UserMachinePage{Items: s.machines, Pagination: api.Pagination{Limit: limit, Total: len(s.machines)}})
		case r.Method == http.MethodGet && r.URL.Path == policyBase:
			_, _ = io.WriteString(w, data(testPolicyJSON))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/workspaces/acme/machines/m1/tags":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.puts = append(s.puts, body)
			if s.refuse && body["confirm"] == nil {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, apiFailure("policy_changes_unconfirmed", `{"access_changes":`+changesJSON+`,"confirm":"sha256:ok"}`))
				return
			}
			tags, _ := json.Marshal(body["tags"])
			_, _ = io.WriteString(w, data(`{"tags":`+string(tags)+`,"policy_version":8}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newTagServer(t *testing.T) (*tagServer, string) {
	s := &tagServer{machines: []api.UserMachine{
		{ID: "m1", Alias: "db", State: "online", Online: true, Tags: []string{"tag:old"}},
		{ID: "m2", Alias: "web", State: "online", Online: true, Tags: []string{"tag:prod"}},
	}}
	server := httptest.NewServer(s.handler(t))
	t.Cleanup(server.Close)
	return s, server.URL
}

func TestMachineTagAddRemoveReviewsAccessChanges(t *testing.T) {
	s, url := newTagServer(t)
	s.refuse = true
	code, out, errOut := runPolicyCLI(t, url, "machine", "tag", "db", "add", "prod", "tag:ci", "--yes")
	if code != 0 || !strings.Contains(out, "+ gained user:bo@example.test machine:db exec") || !strings.Contains(out, "Tags of db: tag:ci, tag:old, tag:prod (policy version 8)") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	if len(s.puts) != 2 || s.puts[1]["confirm"] != "sha256:ok" || s.puts[0]["expected_version"] != float64(7) {
		t.Fatalf("puts = %v", s.puts)
	}
	if got, _ := json.Marshal(s.puts[0]["tags"]); string(got) != `["tag:ci","tag:old","tag:prod"]` {
		t.Fatalf("add sends the full new list with prefixes: %s", got)
	}
	s.refuse = false
	code, out, errOut = runPolicyCLI(t, url, "machine", "tag", "db", "remove", "old", "--json")
	if code != 0 || !strings.Contains(out, `"policy_version": 8`) {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	if got, _ := json.Marshal(s.puts[2]["tags"]); string(got) != `[]` {
		t.Fatalf("removing the last tag sends an empty list: %s", got)
	}
	if code, _, errOut = runPolicyCLI(t, url, "machine", "tag", "db", "add", "Bad Tag"); code == 0 || !strings.Contains(errOut, "tag:<name>") {
		t.Fatalf("an invalid tag is refused before the network: code=%d err=%s", code, errOut)
	}
	if code, out, _ = runPolicyCLI(t, url, "machine", "tag", "db"); code != 0 || !strings.Contains(out, "tag:old") {
		t.Fatalf("pb machine tag <machine> shows its tags: code=%d out=%s", code, out)
	}
}

func TestMachineListShowsAndFiltersTags(t *testing.T) {
	_, url := newTagServer(t)
	code, out, errOut := runPolicyCLI(t, url, "machine", "list")
	if code != 0 || !strings.Contains(out, "TAGS") || !strings.Contains(out, "tag:old") || !strings.Contains(out, "tag:prod") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	code, out, errOut = runPolicyCLI(t, url, "machine", "list", "--tag", "prod")
	if code != 0 || strings.Contains(out, "db ") || !strings.Contains(out, "web") {
		t.Fatalf("--tag prod lists only tagged machines: code=%d out=%s err=%s", code, out, errOut)
	}
}

func TestStatusShowsMachineTags(t *testing.T) {
	var out strings.Builder
	writeStatus(&out, localapi.Snapshot{DaemonState: "ready", Machines: []localapi.MachineStatus{{ID: "m1", Alias: "db", Tags: []string{"tag:ci", "tag:prod"}}, {ID: "m2", Alias: "web"}}})
	if !strings.Contains(out.String(), "  Tags: tag:ci, tag:prod") || strings.Count(out.String(), "Tags:") != 1 {
		t.Fatalf("%s", out.String())
	}
}
