package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/preview"
	"github.com/spf13/cobra"
)

func parsedAllowList(t *testing.T, args ...string) ([]string, error) {
	t.Helper()
	command := &cobra.Command{Use: "x"}
	addViewerAllowFlag(command, "preview")
	if err := command.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return viewerAllowList(command)
}

func TestViewerAllowFlagIsCheckedBeforeAnythingIsCreated(t *testing.T) {
	if list, err := parsedAllowList(t); list != nil || err != nil {
		t.Fatalf("without --allow = %q, %v", list, err)
	}
	list, err := parsedAllowList(t, "--allow", "User@X.com, *@Company.com", "--allow", "b@y.com")
	if err != nil || strings.Join(list, " ") != "user@x.com *@company.com b@y.com" {
		t.Fatalf("list = %q, %v", list, err)
	}
	for _, bad := range []string{"", "*", "*@*", "*@com", "company.com", "a*@company.com", "*@*.company.com", "a@x.com,", "a@x.com,A@X.COM", "a@x.com,*"} {
		list, err := parsedAllowList(t, "--allow="+bad)
		if err == nil || list != nil || !strings.Contains(err.Error(), "--allow") {
			t.Errorf("--allow=%q = %q, %v; want a refusal naming --allow", bad, list, err)
		}
	}
	for _, test := range []struct {
		want string
		args []string
	}{
		{"allow", []string{"preview", "3000", "--allow", "*"}},
		{"allow", []string{"tunnel", "--ephemeral", "3000", "--allow", "*@com"}},
		{"allow", []string{"tunnel", "create", "demo", "--port", "3000", "--allow", "*"}},
		{"allow", []string{"tunnel", "create", "demo", "--from", "tcp://127.0.0.1:22", "--allow", "a@x.com"}},
		// The two flags cannot be combined; pb words every flag conflict this way.
		{"arguments are invalid", []string{"preview", "3000", "--team", "--allow", "a@x.com"}},
		{"arguments are invalid", []string{"tunnel", "create", "demo", "--port", "3000", "--team", "--allow", "a@x.com"}},
	} {
		command := newRootCommand()
		command.SetArgs(test.args)
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("args=%v error=%v, want a refusal containing %q", test.args, err, test.want)
		}
	}
}

func TestTunnelCreateAllowListDigestKeepsTheOldOneWithoutAList(t *testing.T) {
	digest := func(allowed ...string) string {
		value, err := tunnelCreateRequestDigest("demo", "private", "http", "127.0.0.1:3000", 0, nil, allowed)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest() != digest([]string{}...) || digest() == digest("a@x.com") || digest("a@x.com", "*@y.com") != digest("*@y.com", "a@x.com") {
		t.Fatal("the create digest does not follow the allow list")
	}
}

// allowTunnelServer answers a tunnel create with an allow list. putStatus lists the answers of successive policy PUTs.
type allowTunnelServer struct {
	t         *testing.T
	mu        sync.Mutex
	requests  []string
	saved     api.ViewerPolicy
	putStatus []int
	putCode   string
}

func (s *allowTunnelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	tunnel := validCommandTunnel()
	tunnel.Generation, tunnel.ETag, tunnel.AccessMode = 1, `"tunnel:tun_1:1"`, "private"
	answer := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	const policy = "/v1/browser-access/policies/tunnel/tun_1/route_1"
	switch r.Method + " " + r.URL.Path {
	case "POST /v1/tunnels":
		var body api.TunnelCreateInput
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessMode != "private" {
			s.t.Errorf("create = %#v, %v; --allow must create a private tunnel", body, err)
		}
		tunnel.Name = body.Name
		answer(api.TunnelMutation{Tunnel: tunnel, Operation: validCommandOperation("tunnel", tunnel.ID)})
	case "GET /v1/tunnels/tun_1/routes":
		answer(api.TunnelRoutePage{Items: []api.TunnelRoute{validCommandRoute("route_1", "default")}})
	case "GET " + policy:
		answer(s.saved)
	case "PUT " + policy:
		var input api.ViewerPolicyInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Audience != "specific_emails" || input.ExpectedGeneration != s.saved.Generation {
			s.t.Errorf("policy = %#v, %v", input, err)
		}
		status := http.StatusOK
		if len(s.putStatus) > 0 {
			status, s.putStatus = s.putStatus[0], s.putStatus[1:]
		}
		if status >= 300 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": s.putCode, "message": "refused"}})
			return
		}
		s.saved = api.ViewerPolicy{Audience: input.Audience, AllowedEmails: input.AllowedEmails, Configured: true, Generation: input.ExpectedGeneration + 1}
		w.WriteHeader(status)
		answer(s.saved)
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}
}

func runAllowTunnelCreate(t *testing.T, server *allowTunnelServer) (string, error) {
	t.Helper()
	return runAllowTunnelCreateWith(t, tunnelCommandFixture(t, server.ServeHTTP))
}

// runAllowTunnelCreateWith runs the same create again on the same journal when it is given the same dependencies.
func runAllowTunnelCreateWith(t *testing.T, deps tunnelDependencies) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command := tunnelCobraCommandV1()
	command.SetOut(&output)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"create", "demo", "--port", "3000", "--allow", "Guest@Partner.test,*@company.test", "--json"})
	err := command.ExecuteContext(deps.context(t.Context()))
	return output.String(), err
}

func TestTunnelCreateAllowSavesTheListOnItsPrivateRoute(t *testing.T) {
	pause := viewerAllowSavePause
	viewerAllowSavePause = time.Millisecond
	t.Cleanup(func() { viewerAllowSavePause = pause })
	// The first save answers not_found (the route is not listed yet) and the second 202 (edges still converging).
	server := &allowTunnelServer{t: t, putStatus: []int{http.StatusNotFound, http.StatusAccepted}, putCode: "not_found"}
	output, err := runAllowTunnelCreate(t, server)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, `"allowed_viewers":["guest@partner.test","*@company.test"]`) {
		t.Fatalf("output = %s", output)
	}
	if !server.saved.Configured || strings.Join(server.saved.AllowedEmails, " ") != "guest@partner.test *@company.test" {
		t.Fatalf("saved = %#v", server.saved)
	}
	if puts := strings.Count(strings.Join(server.requests, "\n"), "PUT "); puts != 2 {
		t.Fatalf("requests = %v", server.requests)
	}
}

func TestTunnelCreateAllowFailureSaysOnlyTheOwnerCanOpenIt(t *testing.T) {
	server := &allowTunnelServer{t: t, putStatus: []int{http.StatusConflict}, putCode: "native_access_widening_denied"}
	_, err := runAllowTunnelCreate(t, server)
	var partial *TunnelCreateChangedError
	if !errors.As(err, &partial) || partial.Stage != tunnelViewerAllowStage || partial.TunnelID != "tun_1" {
		t.Fatalf("error = %#v", err)
	}
	if slices.Index(server.requests, "PUT /v1/browser-access/policies/tunnel/tun_1/route_1") < 0 || strings.Count(strings.Join(server.requests, "\n"), "PUT ") != 1 {
		t.Fatalf("a final refusal was retried or never sent: %v", server.requests)
	}
	if message := commandFailureMessage(err, classifyCommandFailure(err)); !strings.Contains(message, "only you can open it") || !strings.Contains(message, "tun_1") {
		t.Fatalf("message = %q", message)
	}
}

func TestTunnelCreateAllowRerunSavesTheListTheFirstRunCouldNot(t *testing.T) {
	server := &allowTunnelServer{t: t, putStatus: []int{http.StatusForbidden}, putCode: "forbidden"}
	deps := tunnelCommandFixture(t, server.ServeHTTP)
	if _, err := runAllowTunnelCreateWith(t, deps); err == nil || server.saved.Configured {
		t.Fatalf("first run = %v, saved = %#v", err, server.saved)
	}
	output, err := runAllowTunnelCreateWith(t, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, `"replayed":true`) || strings.Join(server.saved.AllowedEmails, " ") != "guest@partner.test *@company.test" {
		t.Fatalf("rerun output = %s, saved = %#v", output, server.saved)
	}
}

func allowPreviewRun(t *testing.T, refuse bool) (requests []string, stderr, stdout string, runErr error) {
	t.Helper()
	var mu sync.Mutex
	var ownerSessionID string
	saved := api.ViewerPolicy{}
	const policy = "/v1/browser-access/policies/preview/prv_cli_1/prv_cli_1"
	lease := func(state string) map[string]any {
		value := previewCommandLease("prv_cli_1", "machine_cli", ownerSessionID, "foreground", "http", "127.0.0.1:3000", state)
		value["access_mode"], value["route_id"] = "private", "prv_cli_1"
		return value
	}
	server := newPreviewCommandServer(t, func(r *http.Request, body map[string]any) (any, string, int) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/previews":
			if body["access_mode"] != "private" {
				t.Errorf("--allow created access_mode %#v", body["access_mode"])
			}
			ownerSessionID, _ = body["owner_session_id"].(string)
			return lease("connecting"), `"ptv1:preview_lease:cHJ2X2NsaV8x:1"`, http.StatusOK
		case "GET /v1/previews/prv_cli_1":
			return lease("ready"), `"ptv1:preview_lease:cHJ2X2NsaV8x:1"`, http.StatusOK
		case "GET " + policy:
			return saved, "", http.StatusOK
		case "PUT " + policy:
			if refuse {
				return nil, "", http.StatusForbidden
			}
			saved = api.ViewerPolicy{Audience: "specific_emails", AllowedEmails: []string{"*@company.test"}, Configured: true, Generation: 1}
			return saved, "", http.StatusOK
		case "DELETE /v1/previews/prv_cli_1":
			value := lease("stopped")
			value["allocation_state"], value["edge_state"] = "released", "down"
			return value, `"ptv1:preview_lease:cHJ2X2NsaV8x:2"`, http.StatusOK
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, "", http.StatusTeapot
		}
	})
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "test-token"}, server.Client())
	deps := defaultPreviewDependencies()
	deps.client = func(*cobra.Command) (*api.Client, error) { return client, nil }
	deps.machineID = func() (string, error) { return "machine_cli", nil }
	deps.isTerminal = func(*cobra.Command) bool { return false }
	carrier := &cliPreviewCarrier{ready: make(chan struct{})}
	deps.carrier = func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error) {
		return carrier, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := previewCobraCommandV1()
	var out, errOut lockedPreviewWriter
	command.SetOut(&out)
	command.SetErr(&errOut)
	command.SetArgs([]string{"3000", "--allow", "*@company.test"})
	result := make(chan error, 1)
	go func() { result <- command.ExecuteContext(context.WithValue(ctx, previewDependenciesKey{}, deps)) }()
	deadline := time.After(30 * time.Second)
	for !refuse && out.Len() == 0 {
		select {
		case err := <-result:
			t.Fatalf("preview ended before it was published: %v", err)
		case <-deadline:
			t.Fatal("preview was not published")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !refuse {
		cancel()
	}
	select {
	case runErr = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("preview command did not end")
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(requests), string(errOut.Bytes()), string(out.Bytes()), runErr
}

func TestPreviewAllowPublishesAPrivatePreviewWithItsList(t *testing.T) {
	requests, stderr, stdout, err := allowPreviewRun(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(requests, "PUT /v1/browser-access/policies/preview/prv_cli_1/prv_cli_1") {
		t.Fatalf("requests = %v", requests)
	}
	if !strings.Contains(stderr, "you and *@company.test") || !strings.Contains(stdout, "Preview ready") {
		t.Fatalf("stderr = %q, stdout = %q", stderr, stdout)
	}
}

func TestPreviewAllowFailureStopsThePreviewAndNeverPrintsItsAddress(t *testing.T) {
	requests, _, stdout, err := allowPreviewRun(t, true)
	var rejection commandRejection
	if !errors.As(err, &rejection) || rejection.reason != commandRejectPreviewAllowList {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(stdout, "preview.example.test") {
		t.Fatalf("the address of an unshared preview was printed: %q", stdout)
	}
	if !slices.Contains(requests, "DELETE /v1/previews/prv_cli_1") {
		t.Fatalf("the preview was not stopped: %v", requests)
	}
	if message := commandFailureMessage(err, classifyCommandFailure(err)); !strings.Contains(message, "Nobody but you could open it") {
		t.Fatalf("message = %q", message)
	}
}
