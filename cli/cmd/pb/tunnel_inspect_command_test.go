package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

type inspectorTestServer struct {
	server      *httptest.Server
	issued      []map[string]any
	issueStatus int
}

func newInspectorTestServer(t *testing.T, issueStatus int, routes string) (*api.Client, *inspectorTestServer) {
	t.Helper()
	fixture := &inspectorTestServer{issueStatus: issueStatus}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/inspector/credentials", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		fixture.issued = append(fixture.issued, body)
		if fixture.issueStatus != http.StatusOK {
			writer.WriteHeader(fixture.issueStatus)
			_, _ = fmt.Fprint(writer, `{"error":{"code":"inspector_not_authorized","message":"denied"}}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"data":{"credential_id":"iac_test","token":"grant-for-%v","expires_at":"2026-09-08T01:00:00Z"}}`, body["action"])
	})
	mux.HandleFunc("/v1/inspector/targets", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Selector string `json:"selector"`
			Action   string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input.Action != "inspect" && input.Action != "replay" {
			t.Error("discovery lacks exact action")
		}
		var values []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte("["+routes+"]"), &values); err != nil {
			t.Fatal(err)
		}
		targets := make([]map[string]string, 0, len(values))
		for _, v := range values {
			targets = append(targets, map[string]string{"resource_id": "tun_01", "route_id": v.ID, "route_name": v.Name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": targets})
	})
	mux.HandleFunc("DELETE /v1/inspector/credentials/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fixture.server = server
	client := api.New(server.URL, config.Credential{}, server.Client())
	return client, fixture
}

const inspectorTestRouteJSON = `{"schema":"paperboat.preview-tunnel/v1","kind":"route","id":"rte_01","tunnel_id":"tun_01","name":"web","protocol":"http","host_match":{"type":"catch_all"},"origin":{"scheme":"http","address":"127.0.0.1:3000","preserve_host":true},"priority":0,"connect_timeout_ms":10000,"idle_timeout_ms":90000,"max_concurrent_streams":128,"desired_state":"active","generation":7,"etag":"\"route:rte_01:7\""}`

func inspectorTestDaemon(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previousFactory := inspectorClientsForCommand
	inspectorClientsForCommand = func(command *cobra.Command) (*inspectorClients, error) {
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return nil, err
		}
		return &inspectorClients{server: client, ctx: ctx, daemon: inspectorTestCaller{base: server.URL, client: server.Client()}}, nil
	}
	t.Cleanup(func() { inspectorClientsForCommand = previousFactory })
	previousClient := tunnelClientForCommand
	previousResolve := resolveTunnelSelectorForCommand
	previousIssue := inspectorIssueCredential
	t.Cleanup(func() {
		tunnelClientForCommand = previousClient
		resolveTunnelSelectorForCommand = previousResolve
		inspectorIssueCredential = previousIssue
	})
}

// Command contract tests substitute only the native exchange, keeping the
// command's real credential issuance, scope, output and recovery behavior.
type inspectorTestCaller struct {
	base   string
	client *http.Client
}

func (c inspectorTestCaller) do(ctx context.Context, grant, method, path string, body any, query url.Values) ([]byte, int, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path+"?"+query.Encode(), bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Paperboat-Inspector-Grant", grant)
	res, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, inspectorDaemonMaxBytes+1))
	return data, res.StatusCode, err
}

func wireInspectorTestClients(client *api.Client) {
	tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) { return client, nil }
	resolveTunnelSelectorForCommand = func(context.Context, *api.Client, string) (string, error) { return "tun_01", nil }
}

func runInspectorCommand(t *testing.T, root func() *cobra.Command, args ...string) (string, error) {
	t.Helper()
	command := root()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}
