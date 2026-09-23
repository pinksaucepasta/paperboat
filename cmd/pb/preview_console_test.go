package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/spf13/cobra"
)

func TestPreviewConsoleTransferPreservesURLAndCleansCanceledHandoff(t *testing.T) {
	for _, mode := range []string{"success", "cancel", "retry", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			cancelAfterTransfer := mode == "cancel" || mode == "uncertain"
			registry, err := preview.NewRuntimeOwnerSessionRegistry(preview.RuntimeOwnerSessionRegistryConfig{MachineID: "device_cli", RuntimeDone: make(chan struct{})})
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			manager, err := preview.NewOwnerSessionLeaseManager(preview.OwnerSessionLeaseManagerConfig{MachineID: "device_cli", ControlToken: "control_secret", Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			patchCalls := 0
			ownerHandler := &recordingOwnerLeaseHandler{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patchCalls++
					if mode == "retry" && patchCalls == 1 {
						w.WriteHeader(http.StatusConflict)
						return
					}
					if mode == "uncertain" {
						manager.ServeHTTP(httptest.NewRecorder(), r)
						w.WriteHeader(http.StatusBadGateway)
						return
					}
				}
				manager.ServeHTTP(w, r)
			})}
			ownerServer := httptest.NewServer(ownerHandler)
			defer ownerServer.Close()
			ownerClient, err := preview.NewLocalOwnerSessionClient(ownerServer.URL, "control_secret", ownerServer.Client())
			if err != nil {
				t.Fatal(err)
			}
			var ownerDone <-chan struct{}
			apiServer := newPreviewCommandServer(t, func(r *http.Request, body map[string]any) (any, string, int) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/previews" {
					t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
					return nil, "", 500
				}
				owner := body["owner_session_id"].(string)
				ownerDone, err = registry.OwnerSessionDoneForTarget("account_cli", "device_cli", owner, preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"})
				if err != nil {
					t.Error(err)
				}
				return previewCommandLease("prv_background_1", "device_cli", owner, "http", "127.0.0.1:3000", "connecting"), `"ptv1:preview_lease:cHJ2X2JhY2tncm91bmRfMQ:1"`, http.StatusOK
			})
			defer apiServer.Close()
			client := api.New(apiServer.URL, config.Credential{AccessToken: "test-token"}, apiServer.Client())
			carrier := &backgroundOwnerLeaseCarrier{productionOwnerLeaseCarrier: productionOwnerLeaseCarrier{ready: make(chan struct{}), owner: make(chan string, 1)}}
			oldClient, oldMachine, oldCarrier, oldOwner := previewClientForCommand, previewMachineID, newPreviewCarrier, previewOwnerSessionClientForCommand
			oldTerminal, oldConsole := previewInteractiveTerminal, runPreviewConsoleForCommand
			defer func() {
				previewClientForCommand, previewMachineID, newPreviewCarrier, previewOwnerSessionClientForCommand = oldClient, oldMachine, oldCarrier, oldOwner
				previewInteractiveTerminal, runPreviewConsoleForCommand = oldTerminal, oldConsole
			}()
			previewClientForCommand = func(*cobra.Command) (*api.Client, error) { return client, nil }
			previewMachineID = func() (string, error) { return "device_cli", nil }
			newPreviewCarrier = func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error) {
				return carrier, nil
			}
			previewOwnerSessionClientForCommand = func() (*preview.LocalOwnerSessionClient, error) { return ownerClient, nil }
			previewInteractiveTerminal = func(*cobra.Command) bool { return true }
			runPreviewConsoleForCommand = func(command *cobra.Command, lease preview.Lease, target preview.LeaseTarget, visibility string, domains []string, client *api.Client, ended <-chan error, transfer func(context.Context, time.Duration) (time.Time, error)) (previewConsoleResult, error) {
				if lease.State != "ready" || lease.Endpoint == "" {
					t.Fatal("console started before readiness")
				}
				deadline, err := transfer(command.Context(), 30*time.Minute)
				if mode == "retry" {
					if err == nil {
						t.Fatal("failed transfer was confirmed")
					}
					select {
					case <-ownerDone:
						t.Fatal("failed transfer ended foreground")
					default:
					}
					deadline, err = transfer(command.Context(), 30*time.Minute)
				}
				if err != nil {
					return previewConsoleResult{}, err
				}
				if time.Until(deadline) < 29*time.Minute {
					t.Fatal("unexpected transfer deadline")
				}
				select {
				case <-ownerDone:
					t.Fatal("handoff closed dispatch")
				default:
				}
				if cancelAfterTransfer {
					return previewConsoleResult{}, nil
				}
				return previewConsoleResult{action: "background", output: lease.Endpoint}, nil
			}
			command := previewCobraCommandV1()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs([]string{"3000"})
			runErr := command.ExecuteContext(context.Background())
			if mode == "uncertain" {
				if runErr == nil {
					t.Fatal("uncertain handoff not reported")
				}
			} else if runErr != nil {
				t.Fatal(runErr)
			}
			wantPatch := 1
			if mode == "retry" {
				wantPatch = 2
			}
			if ownerHandler.count(http.MethodPost) != 1 || ownerHandler.count(http.MethodPatch) != wantPatch {
				t.Fatalf("unexpected owner calls: %v", ownerHandler.methods())
			}
			if cancelAfterTransfer {
				if ownerHandler.count(http.MethodDelete) != 1 {
					t.Fatal("canceled handoff left owner behind")
				}
				select {
				case <-ownerDone:
				default:
					t.Fatal("canceled handoff left dispatch running")
				}
			} else {
				if ownerHandler.count(http.MethodDelete) != 0 {
					t.Fatal("successful transfer released owner")
				}
				select {
				case <-ownerDone:
					t.Fatal("successful transfer stopped forwarding")
				default:
				}
			}
		})
	}
}

func TestPreviewConsoleControlsAndErrors(t *testing.T) {
	model := previewConsoleModel{lease: preview.Lease{Endpoint: "https://preview.example", State: "ready"}, target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, visibility: "private", input: textinput.New(), perform: func(action, value string) tea.Cmd {
		return func() tea.Msg { return previewConsoleResult{action: action} }
	}}
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	background := next.(previewConsoleModel)
	if background.prompt != "background" || background.input.Value() != "30m" {
		t.Fatal("background duration prompt missing")
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	tunnel := next.(previewConsoleModel)
	if tunnel.prompt != "tunnel" || !strings.Contains(tunnel.View(), "new URL") {
		t.Fatal("durable transition did not disclose URL change")
	}
	model.domains = []string{"app.example.com"}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if cmd != nil || !strings.Contains(next.(previewConsoleModel).status, "cannot be moved") {
		t.Fatal("custom domains silently migrated")
	}
	next, cmd = model.Update(previewConsoleResult{action: "tunnel", err: fmt.Errorf("connector unavailable")})
	if cmd != nil || !strings.Contains(next.(previewConsoleModel).View(), "connector unavailable") {
		t.Fatal("failure terminated live preview")
	}
	model.busy = true
	_, cmd = model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("busy action disabled cancellation")
	}
}

func TestPreviewMakeTunnelWaitsForDurableReadinessAndRetainsRecovery(t *testing.T) {
	for _, failConnector := range []bool{false, true} {
		t.Run(fmt.Sprint(failConnector), func(t *testing.T) {
			h := newWorkflowAcceptanceHarness(t)
			h.commandContext, h.cancel = context.WithCancel(context.Background())
			defer h.cancel()
			if failConnector {
				h.enrollment.fail = fmt.Errorf("connector unavailable")
			}
			handler := h.server.Config.Handler
			h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/operation_workflow") {
					h.events.add("control.operation.ready")
					operation := validCommandOperation("tunnel", "tun_workflow")
					operation.ID = "operation_workflow"
					operation.State = "succeeded"
					operation.Phase = "ready"
					operation.Progress = 100
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"data": operation})
					return
				}
				handler.ServeHTTP(w, r)
			})
			parent := &cobra.Command{}
			output, err := createTunnelFromPreview(h.commandContext, parent, preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"}, "public", "workflow")
			if failConnector {
				if err == nil || !strings.Contains(err.Error(), "pb tunnel connector add tun_workflow") || output != "" {
					t.Fatalf("missing recoverable failure: output=%q error=%v", output, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(output, "tun_workflow") || !strings.Contains(strings.Join(h.events.snapshot(), "|"), "control.operation.ready|workflow.complete") {
					t.Fatalf("returned before readiness: %q %v", output, h.events.snapshot())
				}
			}
			h.assertJournalLockReleased(t)
		})
	}
}

func TestPreviewConsoleResizesSanitizesAndPreservesActionErrors(t *testing.T) {
	model := previewConsoleModel{lease: preview.Lease{Endpoint: "https://preview.example/\x1b]52;c;payload\x07", State: "ready"}, target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, visibility: "private", input: textinput.New(), status: "Try the connector again"}
	next, _ := model.Update(tea.WindowSizeMsg{Width: 28, Height: 12})
	model = next.(previewConsoleModel)
	next, _ = model.Update(previewConsoleStatus{lease: api.PreviewLease{State: "ready", Domains: []api.PreviewDomainSummary{{Hostname: "app.example.com", State: "waiting_dns", Certificate: api.PreviewDomainCertificate{State: "pending"}, Instructions: &api.PreviewDNSInstructions{Records: []api.PreviewDNSRecord{{Type: "CNAME", Name: "app.example.com", Value: "edge.example.com"}}}}}}})
	model = next.(previewConsoleModel)
	if model.status != "Try the connector again" || !strings.Contains(model.content(), "waiting_dns") || !strings.Contains(model.content(), "CNAME") {
		t.Fatal("poll lost domain instructions or action error")
	}
	view := model.View()
	if strings.Contains(view, "payload") || strings.Contains(view, "\x1b") {
		t.Fatal("terminal control sequence was rendered")
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 28 {
			t.Fatalf("line exceeds terminal: %q", line)
		}
	}
	if len(strings.Split(view, "\n")) > 12 {
		t.Fatal("view exceeds terminal height")
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if next.(previewConsoleModel).view.YOffset == 0 {
		t.Fatal("long domain content cannot be scrolled")
	}
	scrolled := next.(previewConsoleModel).View()
	for _, required := range []string{"Status", "Try the connector again", "s stop", "Esc/Ctrl+C"} {
		if !strings.Contains(scrolled, required) {
			t.Fatalf("scroll hid mandatory content %q: %s", required, scrolled)
		}
	}
}

func TestPreviewConsolePreferencesPanelsKeysAndSafetyControls(t *testing.T) {
	doc := preferences.Default()
	doc.TUI.Keys = map[string]string{"preview_open": "x", "preview_background": "y", "preview_tunnel": "z", "preview_stop": "w"}
	doc.TUI.PreviewPanels = []string{"expiry", "target"}
	model := previewConsoleModel{ctx: preferences.WithContext(context.Background(), doc), lease: preview.Lease{Endpoint: "https://preview.example", State: "ready"}, target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, visibility: "private", domains: []string{"app.example.com"}, input: textinput.New(), status: "Action needs retry", perform: func(action, value string) tea.Cmd {
		return func() tea.Msg { return previewConsoleResult{action: action} }
	}}
	content := model.content()
	if strings.Contains(content, "Visibility") || strings.Contains(content, "Domains") || strings.Index(content, "Expires") > strings.Index(content, "Target") {
		t.Fatalf("panels not applied: %s", content)
	}
	for _, required := range []string{"https://preview.example", "Status", "Action needs retry", "x open browser", "y keep in background", "z make tunnel", "w stop"} {
		if !strings.Contains(content, required) {
			t.Fatalf("missing mandatory content %q", required)
		}
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cmd == nil || cmd().(previewConsoleResult).action != "open" || !next.(previewConsoleModel).busy {
		t.Fatal("configured open binding was not dispatched")
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if next.(previewConsoleModel).prompt != "background" {
		t.Fatal("configured background binding was not dispatched")
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if next.(previewConsoleModel).prompt != "" {
		t.Fatal("old binding remains active after override")
	}
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		busy := model
		busy.busy = true
		_, cmd := busy.Update(tea.KeyMsg{Type: key})
		if cmd == nil {
			t.Fatal("busy action removed safety control")
		}
	}
	doc.TUI.PreviewPanels = []string{}
	model.ctx = preferences.WithContext(context.Background(), doc)
	content = model.content()
	if strings.Contains(content, "Target") || strings.Contains(content, "Expires") || !strings.Contains(content, "Status") {
		t.Fatal("explicit empty panels did not preserve only mandatory content")
	}
	doc.TUI.PreviewPanels = nil
	doc.TUI.Density = "compact"
	model.ctx = preferences.WithContext(context.Background(), doc)
	compact := model.content()
	if !strings.Contains(compact, "Visibility") || strings.Contains(compact, "\n\n") {
		t.Fatal("default panels or compact density not applied")
	}
}

func TestPreviewConsoleThemeAndAccentAreContextLocal(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	doc := preferences.Default()
	doc.TUI.Theme = "dark"
	doc.TUI.Accent = "#123456"
	model := previewConsoleModel{ctx: preferences.WithContext(context.Background(), doc), lease: preview.Lease{Endpoint: "https://preview.example", State: "ready"}, width: 80, height: 24, input: textinput.New()}
	colored := model.View()
	if !strings.Contains(colored, "38;2;18;52;86") {
		t.Fatalf("context accent not applied: %q", colored)
	}
	doc.TUI.Theme = "mono"
	mono := model
	mono.ctx = preferences.WithContext(context.Background(), doc)
	if strings.Contains(mono.View(), "38;") || strings.Contains(mono.View(), "48;") {
		t.Fatal("mono theme emitted foreground/background colors")
	}
	if model.View() != colored {
		t.Fatal("another console theme changed the original instance")
	}
}
