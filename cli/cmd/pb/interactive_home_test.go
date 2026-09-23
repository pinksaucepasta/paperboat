package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

func TestInteractiveCommandPreservesConnectionAndIO(t *testing.T) {
	original := newInteractiveRootCommand
	t.Cleanup(func() { newInteractiveRootCommand = original })
	input := strings.NewReader("input")
	var output, diagnostic bytes.Buffer
	parent := newRootCommand()
	parent.SetIn(input)
	parent.SetOut(&output)
	parent.SetErr(&diagnostic)
	_ = parent.PersistentFlags().Set("config", "/selected/config.json")
	_ = parent.PersistentFlags().Set("server", "https://selected.example")
	type contextMarker struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextMarker{}, "parent-value"))
	defer cancel()
	parent.SetContext(ctx)
	sentinel := errors.New("child result")
	newInteractiveRootCommand = func() *cobra.Command {
		child := &cobra.Command{Use: "pb", SilenceErrors: true, SilenceUsage: true, RunE: func(c *cobra.Command, args []string) error {
			if c.Context().Value(contextMarker{}) != "parent-value" || c.Context().Done() != ctx.Done() || c.InOrStdin() != input || c.OutOrStdout() != &output || c.ErrOrStderr() != &diagnostic {
				t.Error("child lost parent context or IO")
			}
			config, _ := c.Flags().GetString("config")
			server, _ := c.Flags().GetString("server")
			if config != "/selected/config.json" || server != "https://selected.example" {
				t.Errorf("connection changed: %q %q", config, server)
			}
			if strings.Join(args, "|") != "operation|two words" {
				t.Errorf("args = %q", args)
			}
			return sentinel
		}}
		child.Flags().String("config", "", "")
		child.Flags().String("server", "", "")
		return child
	}
	if err := executeInteractiveCommand(parent, []string{"operation", "two words"}); !errors.Is(err, sentinel) {
		t.Fatalf("result=%v", err)
	}
}

func TestInteractiveCatalogCoversProductCommands(t *testing.T) {
	items := interactiveCommandItems(newRootCommand(), nil)
	found := map[string]bool{}
	for _, item := range items {
		found[item.ID] = true
		if strings.HasPrefix(item.ID, "__") {
			t.Errorf("internal command exposed: %s", item.ID)
		}
	}
	for _, name := range []string{"preview", "preview stop", "tunnel create", "tunnel route add", "team invite", "transfer status", "config status", "session share"} {
		if !found[name] {
			t.Errorf("missing %s", name)
		}
	}
	for _, item := range interactiveCommandItems(newRootCommand(), []string{"team"}) {
		if !strings.HasPrefix(item.ID, "team ") {
			t.Errorf("wrong group %s", item.ID)
		}
	}
}

func TestInteractiveInvocationValidationDoesNotExecuteShell(t *testing.T) {
	args, err := validatedInteractiveInvocation([]string{"tunnel", "create"}, `demo --from 'http://localhost:3000'`)
	if err != nil || strings.Join(args, "|") != "tunnel|create|demo|--from|http://localhost:3000" {
		t.Fatalf("args=%q err=%v", args, err)
	}
	for _, line := range []string{`"unterminated`, `demo --unknown-option`, ` `} {
		if _, err := validatedInteractiveInvocation([]string{"tunnel", "create"}, line); err == nil {
			t.Errorf("accepted %q", line)
		}
	}
	args, err = validatedInteractiveInvocation([]string{"team", "create"}, `'$(touch /never-execute)'`)
	if err != nil || args[len(args)-1] != "$(touch /never-execute)" {
		t.Fatalf("shell text changed: %q %v", args, err)
	}
}

func TestHomeOutputIsBoundedWithoutFailingMutation(t *testing.T) {
	output := &homeOutput{}
	data := bytes.Repeat([]byte{'x'}, homeOutputLimit+100)
	if n, err := output.Write(data); n != len(data) || err != nil {
		t.Fatalf("write=%d %v", n, err)
	}
	_, _ = output.Write([]byte("later"))
	if len(output.data) != homeOutputLimit || !strings.Contains(output.String(), "Output truncated") {
		t.Fatal("unbounded or silent truncation")
	}
}

func TestHomePromptCancellationIsNavigation(t *testing.T) {
	for _, err := range []error{selector.ErrCanceled, prompt.ErrCanceled} {
		if !interactiveCanceled(err) {
			t.Errorf("cancel shown as failure: %v", err)
		}
	}
	if interactiveCanceled(errors.New("real failure")) {
		t.Fatal("failure hidden")
	}
}

func TestHomeTextResizeAndBack(t *testing.T) {
	model := homeTextModel{title: "Details", content: strings.Repeat("long output ", 40), view: viewport.New(80, 20), width: 80, height: 24}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 24, Height: 10})
	model = updated.(homeTextModel)
	for _, line := range strings.Split(model.View(), "\n") {
		if ansi.StringWidth(line) > 24 {
			t.Errorf("overflow: %q", line)
		}
	}
	updated, quit := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if quit == nil || updated.(homeTextModel).interrupted {
		t.Fatal("Esc did not go back")
	}
	updated, quit = model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil || !updated.(homeTextModel).interrupted {
		t.Fatal("interrupt lost")
	}
}

func TestHomeTextFitsTinyTerminal(t *testing.T) {
	for _, height := range []int{1, 2, 3} {
		model := homeTextModel{title: "Long title", content: "long result\nnext line", view: viewport.New(80, 20), width: 80, height: 24}
		resized, _ := model.Update(tea.WindowSizeMsg{Width: 3, Height: height})
		lines := strings.Split(resized.(homeTextModel).View(), "\n")
		if len(lines) > height {
			t.Fatalf("view has %d rows at height %d", len(lines), height)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > 3 {
				t.Fatal("tiny terminal overflow")
			}
		}
	}
}
