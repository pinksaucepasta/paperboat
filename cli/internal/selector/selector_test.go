package selector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/cli/internal/preferences"
)

func TestPersistentScreenIsOwnedAcrossNestedControls(t *testing.T) {
	var output bytes.Buffer
	endOuter := BeginScreen(&output)
	endInner := BeginScreen(&output)
	if !ScreenActive() {
		t.Fatal("screen is not active")
	}
	endInner()
	if strings.Count(output.String(), enterAlternateScreen) != 1 || strings.Contains(output.String(), leaveAlternateScreen) {
		t.Fatalf("nested screen output=%q", output.String())
	}
	endOuter()
	if ScreenActive() || strings.Count(output.String(), leaveAlternateScreen) != 1 {
		t.Fatalf("closed screen output=%q", output.String())
	}
}

func TestSuspendScreenRestoresExactlyOnce(t *testing.T) {
	var output bytes.Buffer
	end := BeginScreen(&output)
	defer end()
	output.Reset()

	restore := SuspendScreen(&output)
	if !ScreenActive() {
		t.Fatal("suspending screen changed ownership")
	}
	if got := output.String(); got != leaveAlternateScreen {
		t.Fatalf("suspend output=%q", got)
	}
	restore()
	restore()
	if got := output.String(); got != leaveAlternateScreen+enterAlternateScreen {
		t.Fatalf("restore output=%q", got)
	}
}

func TestNestedSuspendScreenEmitsOnePair(t *testing.T) {
	var output bytes.Buffer
	end := BeginScreen(&output)
	defer end()
	output.Reset()

	first := SuspendScreen(&output)
	second := SuspendScreen(&output)
	if got := output.String(); got != leaveAlternateScreen {
		t.Fatalf("nested suspend output=%q", got)
	}
	first()
	if got := output.String(); got != leaveAlternateScreen {
		t.Fatalf("early restore output=%q", got)
	}
	second()
	if got := output.String(); got != leaveAlternateScreen+enterAlternateScreen {
		t.Fatalf("nested restore output=%q", got)
	}
}

func TestLoadingReturnsWorkerError(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	want := errors.New("load failed")
	if got := Loading(context.Background(), "Machines", "Loading", reader, io.Discard, func(context.Context) error { return want }); !errors.Is(got, want) {
		t.Fatalf("loading error=%v", got)
	}
}

func TestChooseRejectsNonTerminalInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	if _, err := Choose(Options{Title: "Choose", Items: []Item{{ID: "one"}}, Stdin: reader, Output: io.Discard}); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("choose error=%v, want ErrNotTerminal", err)
	}
}

func TestLoadingWaitsForCanceledWorker(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		<-started
		_, _ = writer.Write([]byte{3})
	}()
	err = Loading(context.Background(), "Machines", "Loading", reader, io.Discard, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("loading cancellation error=%v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("loading returned before canceled worker finished")
	}
}

func TestLoadingViewUsesSimplePaperboatMark(t *testing.T) {
	model := loadingModel{title: "Machines", detail: "Loading", width: 80, height: 24}
	view := model.View().Content
	if strings.Contains(view, "~~~") || !strings.Contains(view, "▄█▄") || !strings.Contains(view, "Loading") {
		t.Fatalf("loading view=%q", view)
	}
}

func TestLoadingViewFitsNarrowTerminal(t *testing.T) {
	model := loadingModel{title: strings.Repeat("title", 8), detail: strings.Repeat("detail", 8), width: 7, height: 4}
	view := model.View().Content
	if lines := strings.Count(view, "\n") + 1; lines > 4 {
		t.Fatalf("loading lines=%d view=%q", lines, view)
	}
	for _, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > 7 {
			t.Fatalf("loading line width=%d: %q", width, line)
		}
	}
}

func TestModelFiltersMetadataAndWraps(t *testing.T) {
	m := NewModel([]Item{
		{ID: "env_1", Title: "api", Description: "machine · online · darwin/amd64"},
		{ID: "env_2", Title: "laptop", Description: "machine · online · linux/arm64"},
	}, 1)
	m.Type('a')
	m.Type('r')
	m.Type('m')
	if selected, ok := m.Selected(); !ok || selected.ID != "env_2" {
		t.Fatalf("metadata filter selected %+v, %t", selected, ok)
	}
	m.Backspace()
	m.Backspace()
	m.Backspace()
	m.Move(-1)
	if selected, _ := m.Selected(); selected.ID != "env_2" {
		t.Fatalf("wrapped selection = %+v", selected)
	}
}

func TestModelSupportsFuzzySubsequenceFiltering(t *testing.T) {
	m := NewModel([]Item{{ID: "one", Title: "screenshots/final-report.pdf"}, {ID: "two", Title: "notes.txt"}}, 3)
	for _, r := range "rptdf" {
		m.Type(r)
	}
	if selected, ok := m.Selected(); !ok || selected.ID != "one" {
		t.Fatalf("fuzzy selected=%+v ok=%t", selected, ok)
	}
}

func TestModelUsesFZFPathRanking(t *testing.T) {
	m := NewModel([]Item{
		{ID: "random", Title: "fixtures/source/domain/session.go"},
		{ID: "report", Title: "fixtures/archive/ZETA_release_notes.pdf"},
		{ID: "nested", Title: "fixtures/work/ZETA/notes.md"},
	}, 3)
	m.SetFilter("zeta")
	selected, ok := m.Selected()
	if !ok || (selected.ID != "report" && selected.ID != "nested") {
		t.Fatalf("path-ranked selection=%+v ok=%t", selected, ok)
	}
}

func TestModelPrioritizesExactBasenameOverFuzzyPath(t *testing.T) {
	m := NewModel([]Item{
		{ID: "fuzzy", Title: "fixtures/game/mod-loader.jar"},
		{ID: "exact", Title: "fixtures/source/go.mod"},
		{ID: "nested", Title: "fixtures/go/modules/readme.txt"},
	}, 3)
	m.SetFilter("go.mod")
	selected, ok := m.Selected()
	if !ok || selected.ID != "exact" {
		t.Fatalf("exact basename selection=%+v ok=%t", selected, ok)
	}
}

func TestModelCanHideItemsUntilFilteringStarts(t *testing.T) {
	m := NewModel([]Item{{ID: "one", Title: "report.pdf"}}, 3)
	m.requireFilter = true
	m.applyFilter()
	if _, ok := m.Selected(); ok || len(m.visible) != 0 {
		t.Fatal("unfiltered model exposed choices")
	}
	m.Type('r')
	if selected, ok := m.Selected(); !ok || selected.ID != "one" {
		t.Fatalf("filtered selection=%+v ok=%t", selected, ok)
	}
}

func TestChooserUpdatesRequiredFilterFromTypedRunes(t *testing.T) {
	choices := NewModel([]Item{{ID: "one", Title: "report.pdf"}, {ID: "two", Title: "notes.txt"}}, 3)
	choices.requireFilter = true
	choices.applyFilter()
	input := textinput.New()
	input.Focus()
	base := chooserModel{options: Options{RequireFilter: true}, choices: choices, input: input, width: 80, height: 24}
	updated, command := base.Update(tea.KeyPressMsg{Text: "rpt"})
	result := updated.(chooserModel)
	if result.choices.Filter() != "rpt" {
		t.Fatalf("filter=%q command=%v", result.choices.Filter(), command)
	}
	if selected, ok := result.choices.Selected(); !ok || selected.ID != "one" {
		t.Fatalf("selected=%+v ok=%t", selected, ok)
	}
}

func TestModelNoMatchesCannotSelect(t *testing.T) {
	m := NewModel([]Item{{ID: "env_1", Title: "api"}}, 3)
	for _, r := range "missing" {
		m.Type(r)
	}
	if selected, ok := m.Selected(); ok {
		t.Fatalf("unexpected selection %+v", selected)
	}
}

func TestChooserDistinguishesBackFromQuit(t *testing.T) {
	base := chooserModel{options: Options{}, choices: NewModel(nil, 8), width: 80, height: 24}
	updated, command := base.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	back := updated.(chooserModel)
	if command == nil || !back.canceled || back.interrupted {
		t.Fatalf("escape = canceled %t interrupted %t command %v", back.canceled, back.interrupted, command)
	}
	updated, command = base.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	quit := updated.(chooserModel)
	if command == nil || quit.canceled || !quit.interrupted {
		t.Fatalf("ctrl+c = canceled %t interrupted %t command %v", quit.canceled, quit.interrupted, command)
	}
}

func TestChooserSupportsMouseWheelAndClick(t *testing.T) {
	base := chooserModel{options: Options{Header: "Paperboat", Title: "Machines"}, choices: NewModel([]Item{{ID: "one"}, {ID: "two"}}, 8), width: 80, height: 24}
	updated, _ := base.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	scrolled := updated.(chooserModel)
	if selected, _ := scrolled.choices.Selected(); selected.ID != "two" {
		t.Fatalf("wheel selected %q", selected.ID)
	}
	updated, command := base.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 6})
	clicked := updated.(chooserModel)
	if command != nil || clicked.confirmed {
		t.Fatalf("a single click confirmed: confirmed=%t command=%v", clicked.confirmed, command)
	}
	if selected, _ := clicked.choices.Selected(); selected.ID != "two" {
		t.Fatalf("a single click selected %q, want the clicked row", selected.ID)
	}
	updated, command = clicked.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 6})
	double := updated.(chooserModel)
	if command == nil || !double.confirmed || double.selected.ID != "two" {
		t.Fatalf("double click confirmed=%t selected=%q command=%v", double.confirmed, double.selected.ID, command)
	}
	clicked.lastClickAt = time.Now().Add(-2 * doubleClickWindow)
	if updated, command = clicked.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 6}); command != nil || updated.(chooserModel).confirmed {
		t.Fatal("two slow clicks confirmed")
	}
	if updated, command = clicked.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 4}); command != nil || updated.(chooserModel).confirmed {
		t.Fatal("clicks on different rows confirmed")
	}
}

func TestChooserMouseMotionUpdatesHoverSelection(t *testing.T) {
	base := chooserModel{options: Options{Title: "Machines"}, choices: NewModel([]Item{{ID: "one"}, {ID: "two"}}, 8), width: 80, height: 24}
	updated, command := base.Update(tea.MouseMotionMsg{X: 4, Y: 4})
	hovered := updated.(chooserModel)
	if command != nil {
		t.Fatalf("hover command=%v", command)
	}
	if selected, _ := hovered.choices.Selected(); selected.ID != "two" {
		t.Fatalf("hover selected %q", selected.ID)
	}
}

func TestChooserReturnsConfiguredKeyAction(t *testing.T) {
	base := chooserModel{options: Options{Title: "Machines", Actions: map[string]string{"ctrl+f": "favorite"}}, choices: NewModel([]Item{{ID: "one"}}, 8), width: 80, height: 24}
	updated, command := base.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	result := updated.(chooserModel)
	if command == nil || !result.confirmed || result.action != "favorite" || result.selected.ID != "one" {
		t.Fatalf("action=%q selected=%q confirmed=%t command=%v", result.action, result.selected.ID, result.confirmed, command)
	}
}

func TestChooserCanAcceptRecognizedPastedInput(t *testing.T) {
	input := textinput.New()
	input.Focus()
	base := chooserModel{
		options: Options{InputSelection: func(value string) (Item, bool) {
			return Item{ID: "drop", Title: value}, value == "/tmp/report.pdf"
		}},
		choices: NewModel([]Item{{ID: "ordinary"}}, 8), input: input, width: 80, height: 24,
	}
	updated, command := base.Update(tea.KeyPressMsg{Text: "/tmp/report.pdf"})
	result := updated.(chooserModel)
	if !result.confirmed || result.selected.ID != "drop" || command == nil {
		t.Fatalf("result=%#v command=%v", result, command)
	}
}

func TestChooserPreservesUnrecognizedPastedInputAsFilter(t *testing.T) {
	input := textinput.New()
	input.Focus()
	base := chooserModel{options: Options{InputSelection: func(string) (Item, bool) { return Item{}, false }}, choices: NewModel([]Item{{ID: "ordinary"}}, 8), input: input, width: 80, height: 24}
	updated, _ := base.Update(tea.KeyPressMsg{Text: "ordinary text"})
	result := updated.(chooserModel)
	if result.confirmed || result.input.Value() != "ordinary text" || result.choices.Filter() != "ordinary text" {
		t.Fatalf("input=%q filter=%q confirmed=%v", result.input.Value(), result.choices.Filter(), result.confirmed)
	}
}

func TestChooserReturnsHeaderClickAction(t *testing.T) {
	base := chooserModel{options: Options{Header: "Paperboat\nVersion\nmasked", HeaderActions: map[int]string{2: "reveal"}}, choices: NewModel([]Item{{ID: "one"}}, 8), width: 80, height: 24}
	updated, command := base.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 20, Y: 2})
	result := updated.(chooserModel)
	if command == nil || !result.confirmed || result.action != "reveal" {
		t.Fatalf("action=%q confirmed=%t command=%v", result.action, result.confirmed, command)
	}
}

func TestEmptyChooserRendersUsefulState(t *testing.T) {
	model := chooserModel{options: Options{Title: "Public previews", Empty: "No public previews yet"}, choices: NewModel(nil, 8), width: 80, height: 24}
	view := model.View().Content
	if !strings.Contains(view, "No public previews yet") || strings.Contains(view, "No matches") {
		t.Fatalf("empty view = %q", view)
	}
	if _, ok := model.choices.Selected(); ok {
		t.Fatal("empty chooser unexpectedly selected an item")
	}
}

func TestChooserRendersActionItemsDistinctly(t *testing.T) {
	model := chooserModel{
		options: Options{Title: "Machines"},
		choices: NewModel([]Item{{ID: "machine", Title: "Remote"}, {ID: "add", Title: "+ Add machine", Action: true}}, 8),
		width:   80,
		height:  24,
	}
	view := model.View().Content
	if !strings.Contains(view, stylesForContext(model.options.Context).action.Render("     + Add machine")) {
		t.Fatalf("action item was not accented: %q", view)
	}
}

func TestChooserViewFitsNarrowTerminal(t *testing.T) {
	model := chooserModel{
		options: Options{Header: strings.Repeat("header", 8), Title: strings.Repeat("title", 8), Subtitle: strings.Repeat("subtitle", 8), Footer: strings.Repeat("footer", 8)},
		choices: NewModel([]Item{{Title: strings.Repeat("item", 8), Description: strings.Repeat("description", 8)}}, 8),
		width:   7,
		height:  4,
	}
	view := model.View().Content
	if lines := strings.Count(view, "\n") + 1; lines > 4 {
		t.Fatalf("chooser lines=%d view=%q", lines, view)
	}
	for _, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > 7 {
			t.Fatalf("chooser line width=%d: %q", width, line)
		}
	}
}

func TestChooserSanitizesTerminalControlsInItemText(t *testing.T) {
	malicious := "\x1b[31mowned\x1b]8;;https://evil.example\x07click\x1b]8;;\x07"
	model := chooserModel{
		options: Options{Title: "Machines"},
		choices: NewModel([]Item{{Title: malicious, Description: malicious}}, 8),
		width:   80,
		height:  12,
	}
	// v2 styles always emit SGR; only item text may not smuggle other controls.
	view := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(model.View().Content, "")
	if strings.ContainsAny(view, "\x1b\x07") {
		t.Fatalf("item text injected terminal controls: %q", view)
	}
	if !strings.Contains(view, "owned") || !strings.Contains(view, "click") {
		t.Fatalf("sanitizing removed visible item text: %q", view)
	}
}

func TestChooserRendersFavoriteItemsWithAccent(t *testing.T) {
	model := chooserModel{
		options: Options{Title: "Machines"},
		choices: NewModel([]Item{{ID: "plain", Title: "Plain"}, {ID: "favorite", Title: "Favorite", Favorite: true}}, 8),
		width:   80,
		height:  24,
	}
	view := model.View().Content
	styles := stylesForContext(model.options.Context)
	if !strings.Contains(view, styles.favorite.Render("     Favorite")+" "+styles.favoriteMarker.Render("◆")) {
		t.Fatalf("favorite item was not accented: %q", view)
	}
}

func TestSelectorErrorsRemainDistinct(t *testing.T) {
	if errors.Is(ErrCanceled, ErrInterrupted) || errors.Is(ErrInterrupted, ErrCanceled) {
		t.Fatal("back and interrupt errors must remain distinct")
	}
}

func BenchmarkModelFiltersFiftyThousandPaths(b *testing.B) {
	items := make([]Item, 50000)
	for index := range items {
		items[index] = Item{ID: fmt.Sprintf("file-%d", index), Title: fmt.Sprintf("fixtures/project-%05d/source/component.go", index)}
	}
	items[31415] = Item{ID: "zeta", Title: "fixtures/archive/ZETA_release_notes.pdf"}
	model := NewModel(items, 8)
	b.ResetTimer()
	for range b.N {
		model.SetFilter("zeta")
	}
}

func TestLoadingCancellationPreservesWorkerFailureAndRecovery(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{})
	go func() { <-started; _, _ = writer.Write([]byte{3}) }()
	got := Loading(context.Background(), "PRIVATE title", "Loading", reader, io.Discard, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return errors.Join(ctx.Err(), syscall.EIO)
	})
	if !errors.Is(got, ErrCanceled) || !errors.Is(got, syscall.EIO) || strings.Contains(got.Error(), "PRIVATE") {
		t.Fatalf("worker failure lost or unsafe: %v", got)
	}
	if err := Loading(context.Background(), "Machines", "Loading", reader, io.Discard, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestLoadingRetainsErrorPanicWithoutFormatting(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cause := selectorPrivateFailure{}
	got := Loading(context.Background(), "Machines", "Loading", reader, io.Discard, func(context.Context) error { panic(cause) })
	if !errors.Is(got, tea.ErrProgramPanic) || !errors.Is(got, cause) || got.Error() != "interactive selection failed" {
		t.Fatalf("worker panic cause lost: %v", got)
	}
}

type selectorPrivateFailure struct{}

func (selectorPrivateFailure) Error() string { panic("private error must not be formatted") }

func TestViewUsesCellMotionMouse(t *testing.T) {
	if mode := NewView(context.Background(), "x").MouseMode; mode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want cell motion", mode)
	}
}

func TestMousePreferenceOffDisablesMouseReporting(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Mouse: "off"}})
	if mode := NewView(ctx, "x").MouseMode; mode != tea.MouseModeNone {
		t.Fatalf("mouse off left mode %v", mode)
	}
	model := chooserModel{options: Options{Context: ctx, Title: "Machines"}, choices: NewModel([]Item{{ID: "one"}}, 8), width: 80, height: 24}
	if mode := model.View().MouseMode; mode != tea.MouseModeNone {
		t.Fatalf("chooser ignores the mouse preference: %v", mode)
	}
	invalid := preferences.Document{Version: 1, TUI: preferences.TUI{Mouse: "sometimes"}}
	if err := preferences.Validate(invalid); err == nil {
		t.Fatal("an unknown mouse value was accepted")
	}
}

func TestFilterLineHasNoTruncationMarker(t *testing.T) {
	model := chooserModel{options: Options{Title: "t"}, choices: NewModel([]Item{{ID: "a", Title: "a"}}, 5), input: textinput.New(), width: 100, height: 20}
	model.input.Focus()
	if view := model.render(); strings.Contains(view, "...") {
		t.Fatalf("filter line truncated: %q", view)
	}
}

func TestChooserTooSmallShowsHintAndNeverSelects(t *testing.T) {
	items := []Item{{ID: "one", Title: "One"}}
	for _, size := range [][2]int{{30, 24}, {80, 4}, {1, 1}} {
		model := chooserModel{options: Options{Title: "Machines"}, choices: NewModel(items, 8), width: size[0], height: size[1]}
		view := ansi.Strip(model.View().Content)
		if size[0] >= 30 && !strings.Contains(view, "Enlarge terminal") {
			t.Errorf("%v: no hint in %q", size, view)
		}
		if ansi.StringWidth(view) > size[0] || strings.Contains(view, "\n") {
			t.Errorf("%v: hint overflows or wraps: %q", size, view)
		}
		updated, quit := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if quit != nil || updated.(chooserModel).confirmed {
			t.Errorf("%v: Enter selected an unreadable menu", size)
		}
		updated, _ = model.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 3})
		if updated.(chooserModel).confirmed {
			t.Errorf("%v: a click selected an unreadable menu", size)
		}
		if updated, quit = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); quit == nil || !updated.(chooserModel).canceled {
			t.Errorf("%v: Esc must still go back", size)
		}
		if _, quit = model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); quit == nil {
			t.Errorf("%v: Ctrl+C must still interrupt", size)
		}
	}
	model := chooserModel{options: Options{Title: "Machines"}, choices: NewModel(items, 8), width: 40, height: 5}
	if strings.Contains(model.View().Content, "Enlarge terminal") {
		t.Fatal("the smallest supported size showed the hint")
	}
}

func TestTruncateLineNeverWiderThanTheScreen(t *testing.T) {
	for width := 1; width <= 4; width++ {
		if got := ansi.StringWidth(truncateLine("a long line of text", width)); got > width {
			t.Errorf("width %d: truncated line is %d wide", width, got)
		}
	}
}

func TestChooserKeepsLastSizeWhenTerminalReportsNone(t *testing.T) {
	model := chooserModel{options: Options{Title: "Machines"}, choices: NewModel([]Item{{ID: "one"}}, 8), width: 80, height: 24}
	updated, _ := model.Update(tea.WindowSizeMsg{})
	if got := updated.(chooserModel); got.width != 80 || got.height != 24 || got.tooSmall() {
		t.Fatalf("a 0x0 report resized the menu to %dx%d", got.width, got.height)
	}
}

func manyItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{ID: fmt.Sprintf("id%d", i), Title: fmt.Sprintf("machine-%04d", i)}
	}
	return items
}

func TestSetItemsReusesFilterBuffersAndSanitizesOnce(t *testing.T) {
	m := NewModel(manyItems(50), 8)
	if len(m.workerSlabs) != 0 {
		t.Fatal("fzf slabs were allocated before any search")
	}
	m.SetFilter("machine-004")
	slab, matches := m.workerSlabs[0], cap(m.matches)
	items := manyItems(50)
	items[7].Title = "\x1b[31mmachine-0047\x1b]8;;x\x07"
	m.SetItems(items)
	if m.workerSlabs[0] != slab || cap(m.matches) < matches {
		t.Fatal("a refresh rebuilt the filter buffers")
	}
	if m.items[7].Title != "machine-0047" || m.primary[7] != "machine-0047" {
		t.Fatalf("SetItems stored unsanitized text: %q / %q", m.items[7].Title, m.primary[7])
	}
	if item, ok := m.Selected(); !ok || item.ID == "" || m.Filter() != "machine-004" {
		t.Fatalf("the filter was lost across SetItems: %q %v", m.Filter(), ok)
	}
}

func TestLargeListFiltersAfterDebounceAndFlushesBeforeSelecting(t *testing.T) {
	model := chooserModel{options: Options{Title: "Machines"}, choices: NewModel(manyItems(debounceAbove+100), 8), input: textinput.New(), width: 80, height: 24}
	model.input.Focus()
	var next tea.Model = model
	var command tea.Cmd
	for _, r := range "0599" {
		next, command = next.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	typed := next.(chooserModel)
	if len(typed.choices.visible) != debounceAbove+100 || command == nil || !typed.filterPending {
		t.Fatalf("filter ran per keystroke on a large list: visible=%d pending=%v", len(typed.choices.visible), typed.filterPending)
	}
	if stale, _ := typed.Update(filterDebounced{seq: typed.filterSeq - 1}); len(stale.(chooserModel).choices.visible) != debounceAbove+100 {
		t.Fatal("a superseded debounce ran the filter")
	}
	done, _ := typed.Update(filterDebounced{seq: typed.filterSeq})
	if got := done.(chooserModel); len(got.choices.visible) != 1 || got.filterPending {
		t.Fatalf("debounce did not apply the filter: visible=%d", len(got.choices.visible))
	}
	entered, quit := typed.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := entered.(chooserModel); quit == nil || got.selected.Title != "machine-0599" {
		t.Fatalf("Enter selected %q from a stale list", got.selected.Title)
	}
	small := chooserModel{options: Options{Title: "Machines"}, choices: NewModel(manyItems(20), 8), input: textinput.New(), width: 80, height: 24}
	small.input.Focus()
	next, _ = small.Update(tea.KeyPressMsg{Code: '0', Text: "0"})
	next, _ = next.Update(tea.KeyPressMsg{Code: '0', Text: "0"})
	next, _ = next.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	next, _ = next.Update(tea.KeyPressMsg{Code: '5', Text: "5"})
	if got := next.(chooserModel); len(got.choices.visible) != 1 || got.filterPending {
		t.Fatalf("a short list must filter on every keystroke: visible=%d", len(got.choices.visible))
	}
}

func TestLoadingSpinnerAdvancesThroughBubblesSpinner(t *testing.T) {
	model := loadingModel{spin: spinner.New(spinner.WithSpinner(spinner.Line)), title: "Machines", detail: "Loading", width: 40, height: 10}
	var next tea.Model = model
	seen := map[string]bool{}
	for range 4 {
		view := ansi.Strip(next.(loadingModel).render())
		seen[strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.Split(view, "\n")[6]), "Loading"))] = true
		var command tea.Cmd
		next, command = next.Update(next.(loadingModel).spin.Tick())
		if command == nil {
			t.Fatal("the spinner stopped ticking")
		}
	}
	if len(seen) != 4 {
		t.Fatalf("spinner showed %d distinct frames, want the 4 line frames: %v", len(seen), seen)
	}
}

// canceledLike is how the net package reports a canceled dial: not context.Canceled
// itself, but Is-equal to it.
type canceledLike struct{}

func (canceledLike) Error() string        { return "operation was canceled" }
func (canceledLike) Is(target error) bool { return target == context.Canceled }

func TestLoadingEscStaysCleanWhenWorkerReportsItsOwnCancellation(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{})
	go func() {
		<-started
		_, _ = writer.Write([]byte{27})
	}()
	err = Loading(context.Background(), "Machines", "Loading", reader, io.Discard, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return fmt.Errorf("list machines: %w", &net.OpError{Op: "dial", Net: "tcp", Err: canceledLike{}})
	})
	var failure selectorFailure
	if errors.As(err, &failure) || !errors.Is(err, ErrCanceled) {
		t.Fatalf("Esc during a slow cancel became a failure: %v", err)
	}
}

func TestLoadingKeepsRealWorkerFailureAfterEsc(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{})
	go func() {
		<-started
		_, _ = writer.Write([]byte{27})
	}()
	real := errors.New("disk full")
	err = Loading(context.Background(), "Machines", "Loading", reader, io.Discard, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return errors.Join(canceledLike{}, real)
	})
	if !errors.Is(err, real) {
		t.Fatalf("a real failure was hidden behind the cancel: %v", err)
	}
}

func TestNoticeShowsInFooterAndFades(t *testing.T) {
	SetNotice("Paused tunnel demo\x1b[31m")
	model := chooserModel{options: Options{Title: "Machines"}, choices: NewModel([]Item{{ID: "one", Title: "One"}}, 8), width: 80, height: 24, notice: takeNotice()}
	view := ansi.Strip(model.View().Content)
	lines := strings.Split(view, "\n")
	if got := strings.TrimSpace(lines[len(lines)-1]); got != "Paused tunnel demo" {
		t.Fatalf("footer = %q, want the sanitized notice", got)
	}
	if takeNotice() != "" {
		t.Fatal("a notice was shown twice")
	}
	if command := model.Init(); command == nil {
		t.Fatal("a notice must schedule its fade")
	}
	faded, _ := model.Update(noticeExpired{})
	if strings.Contains(ansi.Strip(faded.(chooserModel).View().Content), "Paused tunnel") {
		t.Fatal("the notice outlived its timer")
	}
	pressed, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if strings.Contains(ansi.Strip(pressed.(chooserModel).View().Content), "Paused tunnel") {
		t.Fatal("the notice outlived the next key press")
	}
	pendingNotice.Store(&queuedNotice{text: "old", at: time.Now().Add(-time.Hour)})
	if takeNotice() != "" {
		t.Fatal("a stale notice was shown")
	}
}
