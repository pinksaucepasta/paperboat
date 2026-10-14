package main

import (
	"bytes"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"strings"
	"testing"
)

func TestInventoryContinuationPreservesInvocationAndFilters(t *testing.T) {
	root := newRootCommand()
	command, _, err := root.Find([]string{"tunnel", "route", "list"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := writeInventoryContinuation(command, []string{"tun_1"}, "a cursor", 50); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "pb tunnel route list tun_1 --cursor") || !strings.Contains(text, "--limit 50") {
		t.Fatalf("continuation=%q", text)
	}
}
func TestHomePreferenceIDsMatchActualRows(t *testing.T) {
	allowed := map[string]bool{}
	for _, id := range preferences.HomeIDs() {
		allowed[id] = true
	}
	for _, item := range homeItems() {
		if !allowed[item.ID] {
			t.Errorf("home row %s cannot be customized", item.ID)
		}
		delete(allowed, item.ID)
	}
	for id := range allowed {
		t.Errorf("stale home preference %s", id)
	}
}
func TestPreviewListExposesPaginationAndFilters(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"preview", "list"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cursor", "limit", "q", "state", "owner"} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("missing %s", name)
		}
	}
}

func TestHomeSessionActionsRespectLifecycleAndSharing(t *testing.T) {
	for _, scenario := range []struct {
		state     string
		isDefault bool
		allowed   []string
	}{{"running", false, []string{"attach", "sharing", "rename", "close"}}, {"closed", false, []string{"rename", "delete"}}, {"closed", true, []string{"rename"}}} {
		actions := homeSessionActions(api.TerminalSession{State: scenario.state, IsDefault: scenario.isDefault})
		if len(actions) != len(scenario.allowed) {
			t.Fatalf("state=%s actions=%v", scenario.state, actions)
		}
		for i, action := range actions {
			if action.ID != scenario.allowed[i] {
				t.Errorf("state=%s action=%s want %s", scenario.state, action.ID, scenario.allowed[i])
			}
		}
	}
}

func TestInteractiveForegroundCommandsKeepTheirTerminal(t *testing.T) {
	for _, args := range [][]string{{"new"}, {"session"}, {"machine"}, {"environments"}, {"daemon"}, {"daemon", "run"}, {"daemon", "machine-guard", "run"}, {"send", "file.txt", "--to", "target"}, {"rsync", "source", "target:"}} {
		if !interactiveStreaming(args) {
			t.Errorf("foreground command captured: %v", args)
		}
	}
	for _, args := range [][]string{{"send", "list"}, {"session", "list"}, {"daemon", "machine-guard", "install"}} {
		if interactiveStreaming(args) {
			t.Errorf("finite command incorrectly streamed: %v", args)
		}
	}
}

func TestSelectedDiagnosticsProvideRecovery(t *testing.T) {
	for _, id := range []string{"setup", "identity", "credential", "inbox", "runtime", "workloads", "auth", "backend"} {
		detail := homeDoctorDetail(selector.Item{ID: id, Description: "current state"})
		if !strings.Contains(detail, "current state") || !strings.Contains(detail, "pb doctor --json") {
			t.Fatalf("missing detail/recovery for %s", id)
		}
	}
}

func TestComparisonFailureKeepsPrivateCauseAndReadRecovery(t *testing.T) {
	cause := errors.New("private content must not appear")
	failure := &configComparisonFailure{cause: cause}
	if !errors.Is(failure, cause) {
		t.Fatal("read failure lost its cause")
	}
	message := userFacingError(failure)
	if strings.Contains(message, cause.Error()) || !strings.Contains(message, "pb config status") {
		t.Fatalf("unsafe or unactionable read error: %q", message)
	}
	output := classifyCLIJSONError(failure)
	if output.StateChanged != false || output.Code != "config_comparison_unavailable" || strings.Contains(output.Message, cause.Error()) {
		t.Fatalf("invalid read error contract: %+v", output)
	}
}
