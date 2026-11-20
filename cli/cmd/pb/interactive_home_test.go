package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/cli/internal/prompt"
	"github.com/pinksaucepasta/paperboat/cli/internal/selector"
	"github.com/spf13/cobra"
)

func TestInteractiveCommandPreservesConnectionAndIO(t *testing.T) {
	input := strings.NewReader("input")
	var output, diagnostic bytes.Buffer
	parent := newRootCommand()
	parent.SetIn(input)
	parent.SetOut(&output)
	parent.SetErr(&diagnostic)
	_ = parent.PersistentFlags().Set("config", "/selected/config.json")
	_ = parent.PersistentFlags().Set("server", "https://selected.example")
	type contextMarker struct{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sentinel := errors.New("child result")
	newRoot := func() *cobra.Command {
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
	parent.SetContext(withHomeDependencies(context.WithValue(ctx, contextMarker{}, "parent-value"), func(d *homeDependencies) { d.newRoot = newRoot }))
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
	for _, name := range []string{"preview", "preview stop", "tunnel create", "tunnel route add", "team invite", "send status", "config status", "session share"} {
		if !found[name] {
			t.Errorf("missing %s", name)
		}
	}
	for _, item := range interactiveCommandItems(newRootCommand(), []string{"team"}) {
		if item.ID != "team" && !strings.HasPrefix(item.ID, "team ") {
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
	model := homeTextModel{title: "Details", content: strings.Repeat("long output ", 40), view: viewport.New(viewport.WithWidth(80), viewport.WithHeight(20)), width: 80, height: 24}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 24, Height: 10})
	model = updated.(homeTextModel)
	for _, line := range strings.Split(model.View().Content, "\n") {
		if ansi.StringWidth(line) > 24 {
			t.Errorf("overflow: %q", line)
		}
	}
	updated, quit := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if quit == nil || updated.(homeTextModel).interrupted {
		t.Fatal("Esc did not go back")
	}
	updated, quit = model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if quit == nil || !updated.(homeTextModel).interrupted {
		t.Fatal("interrupt lost")
	}
}

// Menus type "q" into their filter, so the viewer one Esc away must not quit on it.
func TestHomeTextQDoesNotQuit(t *testing.T) {
	model := homeTextModel{title: "Details", content: "result", view: viewport.New(viewport.WithWidth(80), viewport.WithHeight(20)), width: 80, height: 24}
	if _, quit := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); quit != nil {
		t.Fatal("q quit the result viewer")
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: tea.KeyEscape}} {
		if _, quit := model.Update(key); quit == nil {
			t.Fatalf("%v no longer leaves the viewer", key)
		}
	}
}

func TestHomeTextFitsTinyTerminal(t *testing.T) {
	for _, height := range []int{1, 2, 3} {
		model := homeTextModel{title: "Long title", content: "long result\nnext line", view: viewport.New(viewport.WithWidth(80), viewport.WithHeight(20)), width: 80, height: 24}
		resized, _ := model.Update(tea.WindowSizeMsg{Width: 3, Height: height})
		lines := strings.Split(resized.(homeTextModel).View().Content, "\n")
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

func TestHomeMutationFailureRemainsFailureAfterDisplay(t *testing.T) {
	sentinel := invocationError(errors.New("mutation rejected; state unchanged"))
	displayed := 0
	ctx := withHomeDependencies(t.Context(), func(d *homeDependencies) {
		d.newRoot = func() *cobra.Command {
			root := &cobra.Command{Use: "pb", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().Bool("no-customization", false, "")
			root.AddCommand(&cobra.Command{Use: "mutate", RunE: func(command *cobra.Command, args []string) error { return sentinel }})
			return root
		}
		d.showText = func(command *cobra.Command, title, content string) error {
			displayed++
			if !strings.Contains(content, userFacingError(sentinel)) {
				t.Errorf("failure omitted: %q", content)
			}
			return nil
		}
	})
	parent := &cobra.Command{Use: "pb"}
	parent.SetContext(ctx)
	parent.Flags().Bool("no-customization", true, "")
	var diagnostic bytes.Buffer
	parent.SetErr(&diagnostic)
	err := runHomeResult(parent, []string{"mutate"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("mutation failure became success: %v", err)
	}
	if err := showHomeFailure(parent, err); err != nil {
		t.Fatal(err)
	}
	if displayed != 1 {
		t.Fatalf("failure displayed %d times", displayed)
	}
}

// Tunnel delete from the home menu must ask in the TUI (naming the tunnel), not
// print a confirm code the menu cannot pass back.
func TestConfirmingHomeResultAsksInsteadOfPrintingACode(t *testing.T) {
	var seen []any
	ctx := withHomeDependencies(t.Context(), func(d *homeDependencies) {
		d.showText = func(*cobra.Command, string, string) error { return nil }
		d.newRoot = func() *cobra.Command {
			root := &cobra.Command{Use: "pb", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().Bool("no-customization", false, "")
			root.AddCommand(&cobra.Command{Use: "mutate", RunE: func(command *cobra.Command, _ []string) error {
				seen = append(seen, command.Context().Value(homeInteractiveConfirmation{}))
				return nil
			}})
			return root
		}
	})
	parent := &cobra.Command{Use: "pb"}
	parent.SetContext(ctx)
	parent.Flags().Bool("no-customization", true, "")
	parent.SetErr(&bytes.Buffer{})
	if err := runConfirmingHomeResult(parent, []string{"mutate"}); err != nil {
		t.Fatal(err)
	}
	if err := runHomeResult(parent, []string{"mutate"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != true || seen[1] != nil {
		t.Fatalf("confirmation marker per run = %v, want [true <nil>]", seen)
	}
	if parent.Context().Value(homeInteractiveConfirmation{}) != nil {
		t.Fatal("the marker leaked into the parent context")
	}
}

// A one-line success is a notice on the next menu; longer output and failures are
// still results to read.
func TestHomeResultShowsOneLineSuccessAsNoticeNotViewer(t *testing.T) {
	var output string
	var viewed []string
	ctx := withHomeDependencies(t.Context(), func(d *homeDependencies) {
		d.newRoot = func() *cobra.Command {
			root := &cobra.Command{Use: "pb", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().Bool("no-customization", false, "")
			root.AddCommand(&cobra.Command{Use: "mutate", RunE: func(command *cobra.Command, _ []string) error {
				_, _ = command.OutOrStdout().Write([]byte(output))
				return nil
			}})
			return root
		}
		d.showText = func(_ *cobra.Command, _, content string) error { viewed = append(viewed, content); return nil }
	})
	parent := &cobra.Command{Use: "pb"}
	parent.SetContext(ctx)
	parent.Flags().Bool("no-customization", true, "")
	parent.SetErr(&bytes.Buffer{})
	for out, wantViewer := range map[string]bool{"Paused tunnel tun_1; status: paused\n": false, "": false, "line one\nline two\n": true, strings.Repeat("x", 120): true} {
		output, viewed = out, nil
		if err := runHomeResult(parent, []string{"mutate"}); err != nil {
			t.Fatal(err)
		}
		if (len(viewed) == 1) != wantViewer {
			t.Errorf("output %q: viewer shown %d times, want shown=%v", out, len(viewed), wantViewer)
		}
	}
}
