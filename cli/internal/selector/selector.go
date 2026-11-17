// Package selector provides Paperboat's shared interactive list selection UI.
package selector

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
	"github.com/pinksaucepasta/paperboat/cli/internal/preferences"
)

func init() {
	algo.Init("path")
}

var ErrCanceled = errors.New("selection canceled")
var ErrInterrupted = errors.New("selection interrupted")
var ErrNotTerminal = errors.New("interactive terminal required")

var persistentScreen atomic.Int32
var suspendedScreen atomic.Int32

const enterAlternateScreen = "\x1b[?1049h\x1b[2J\x1b[H\x1b[?25l"
const leaveAlternateScreen = "\x1b[?25h\x1b[?1049l"

// BeginScreen keeps a hierarchy of selectors, prompts, and loading states on
// one alternate screen. The returned function must be called exactly once.
func BeginScreen(output io.Writer) func() {
	if output == nil {
		output = os.Stderr
	}
	if persistentScreen.Add(1) == 1 {
		backgroundAsked.Store(false)
		writeScreen(output, enterAlternateScreen)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if persistentScreen.Add(-1) == 0 {
				writeScreen(output, leaveAlternateScreen)
			}
		})
	}
}

// SuspendScreen temporarily returns the terminal to its normal screen while
// keeping the Paperboat screen owned by the caller. This is useful when a
// nested action needs to run a regular terminal command. The returned restore
// function is safe to call more than once.
func SuspendScreen(output io.Writer) func() {
	if output == nil {
		output = os.Stderr
	}
	if persistentScreen.Load() <= 0 {
		return func() {}
	}
	if suspendedScreen.Add(1) == 1 {
		writeScreen(output, leaveAlternateScreen)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if suspendedScreen.Add(-1) == 0 && persistentScreen.Load() > 0 {
				writeScreen(output, enterAlternateScreen)
			}
		})
	}
}

// RequireTerminal rejects redirected input before Bubble Tea can try to
// recover a controlling /dev/tty or wait forever on a non-interactive stream.
func RequireTerminal(input *os.File) error {
	if input == nil || !term.IsTerminal(input.Fd()) {
		return ErrNotTerminal
	}
	return nil
}

// ProgramOptions makes other Bubble Tea controls participate in the active
// Paperboat screen instead of briefly restoring the normal terminal.
func ProgramOptions(input *os.File, output io.Writer) []tea.ProgramOption {
	if output == nil {
		output = os.Stderr
	}
	programOutput := output
	if fenced, ok := output.(*selectorOutput); ok {
		if file, ok := fenced.Writer.(*os.File); ok && term.IsTerminal(file.Fd()) {
			programOutput = selectorTerminalOutput{selectorOutput: fenced, file: file}
		}
	}
	options := []tea.ProgramOption{tea.WithInput(input), tea.WithOutput(programOutput)}
	if persistentScreen.Load() > 0 && suspendedScreen.Load() == 0 {
		writeScreen(output, "\x1b[2J\x1b[H")
	}
	return options
}

// NewView wraps rendered content with the screen settings v1 passed as program
// options: cell-motion mouse (clicks, drags and wheel only), and the alternate screen unless a persistent
// screen is already active.
func NewView(ctx context.Context, content string) tea.View {
	view := tea.NewView(content)
	if preferences.FromContext(ctx).TUI.Mouse != "off" {
		view.MouseMode = tea.MouseModeCellMotion
	}
	view.AltScreen = persistentScreen.Load() == 0 || suspendedScreen.Load() > 0
	return view
}

func ScreenActive() bool { return persistentScreen.Load() > 0 }

type Item struct {
	ID          string
	Title       string
	Description string
	Search      string
	Action      bool
	Favorite    bool
}

type Options struct {
	Context        context.Context
	Refresh        func(context.Context) ([]Item, error)
	Header         string
	Title          string
	Subtitle       string
	Items          []Item
	Empty          string
	Stdin          *os.File
	Output         io.Writer
	Initial        string
	Footer         string
	Actions        map[string]string
	RequireFilter  bool
	HeaderActions  map[int]string
	InputSelection func(string) (Item, bool)
}

type Result struct {
	Item   Item
	Action string
}

type rankedMatch struct {
	index, class, score, start, length int
}

type Model struct {
	items         []Item
	primary       []string
	searchable    []string
	searchChars   []util.Chars
	matches       []rankedMatch
	workerMatches [][]rankedMatch
	workerSlabs   []*util.Slab
	visible       []int
	filter        []rune
	selected      int
	offset        int
	rows          int
	requireFilter bool
}

// NewModel sanitizes the items once; rendering and filtering then use them as they are.
func NewModel(items []Item, rows int) *Model {
	m := &Model{rows: max(1, rows)}
	m.SetItems(items)
	return m
}

// SetItems replaces the rows (sanitizing each once) and re-applies the filter. The
// per-row search buffers and the fzf slabs are reused, so a refresh allocates only
// for rows that are new.
func (m *Model) SetItems(items []Item) {
	m.items = slices.Grow(m.items[:0], len(items))
	m.primary, m.searchable = m.primary[:0], m.searchable[:0]
	m.searchChars = m.searchChars[:0]
	for _, item := range items {
		item.Title, item.Description, item.Search = SanitizeText(item.Title), SanitizeText(item.Description), SanitizeText(item.Search)
		search := strings.ToLower(strings.TrimSpace(strings.Join([]string{item.Title, item.Search, item.Description}, " ")))
		m.items = append(m.items, item)
		m.primary = append(m.primary, strings.ToLower(item.Title))
		m.searchable = append(m.searchable, search)
		m.searchChars = append(m.searchChars, util.ToChars([]byte(search)))
	}
	m.applyFilter()
}

const maxFilterWorkers = 8

func (m *Model) applyFilter() {
	query := strings.ToLower(string(m.filter))
	m.visible = m.visible[:0]
	if m.requireFilter && query == "" {
		m.selected, m.offset = 0, 0
		return
	}
	if query == "" {
		for index := range m.items {
			m.visible = append(m.visible, index)
		}
	} else {
		pattern := []rune(query)
		m.matches = m.matches[:0]
		workerCount := min(max(1, runtime.GOMAXPROCS(0)), maxFilterWorkers, max(1, len(m.items)))
		for len(m.workerSlabs) < workerCount { // allocated on the first search, then kept
			m.workerSlabs = append(m.workerSlabs, util.MakeSlab(100*1024, 2048))
			m.workerMatches = append(m.workerMatches, nil)
		}
		var workers sync.WaitGroup
		for workerIndex := range workerCount {
			workers.Add(1)
			go func(workerIndex int) {
				defer workers.Done()
				matches := m.workerMatches[workerIndex][:0]
				for index := workerIndex; index < len(m.searchChars); index += workerCount {
					result, _ := algo.FuzzyMatchV2(false, false, true, &m.searchChars[index], pattern, false, m.workerSlabs[workerIndex])
					if result.Start >= 0 {
						matches = append(matches, rankedMatch{index: index, class: matchClass(m.primary[index], query), score: result.Score, start: result.Start, length: len(m.searchable[index])})
					}
				}
				m.workerMatches[workerIndex] = matches
			}(workerIndex)
		}
		workers.Wait()
		for _, matches := range m.workerMatches[:workerCount] {
			m.matches = append(m.matches, matches...)
		}
		slices.SortFunc(m.matches, func(a, b rankedMatch) int {
			if a.class != b.class {
				return a.class - b.class
			}
			if a.score != b.score {
				return b.score - a.score
			}
			if a.length != b.length {
				return a.length - b.length
			}
			if a.start != b.start {
				return a.start - b.start
			}
			return a.index - b.index
		})
		for _, match := range m.matches {
			m.visible = append(m.visible, match.index)
		}
	}
	if m.selected >= len(m.visible) {
		m.selected = max(0, len(m.visible)-1)
	}
	m.ensureVisible()
}

func matchClass(primary, query string) int {
	normalized := strings.ReplaceAll(primary, "\\", "/")
	base := normalized
	if separator := strings.LastIndexByte(normalized, '/'); separator >= 0 {
		base = normalized[separator+1:]
	}
	if strings.HasPrefix(base, query) {
		return 0
	}
	if strings.Contains(base, query) {
		return 1
	}
	if strings.Contains(normalized, query) {
		return 2
	}
	return 3
}

func (m *Model) ensureVisible() {
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+m.rows {
		m.offset = m.selected - m.rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) Move(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.selected = (m.selected + delta + len(m.visible)) % len(m.visible)
	m.ensureVisible()
}

func (m *Model) Type(r rune) {
	if r == 0 || unicode.IsControl(r) {
		return
	}
	m.filter = append(m.filter, r)
	m.selected, m.offset = 0, 0
	m.applyFilter()
}

func (m *Model) Backspace() {
	if len(m.filter) == 0 {
		return
	}
	m.filter = m.filter[:len(m.filter)-1]
	m.selected, m.offset = 0, 0
	m.applyFilter()
}

func (m *Model) SetFilter(value string) {
	m.filter = []rune(value)
	m.selected, m.offset = 0, 0
	m.applyFilter()
}

func (m *Model) Selected() (Item, bool) {
	if len(m.visible) == 0 {
		return Item{}, false
	}
	return m.items[m.visible[m.selected]], true
}

func (m *Model) Filter() string { return string(m.filter) }

func Choose(options Options) (selected Item, err error) {
	result, err := ChooseWithAction(options)
	return result.Item, err
}

func ChooseWithAction(options Options) (selection Result, err error) {
	if options.Stdin == nil {
		options.Stdin = os.Stdin
	}
	if err := RequireTerminal(options.Stdin); err != nil {
		return Result{}, err
	}
	options.Header = sanitizeHeader(options.Header)
	options.Title = SanitizeText(options.Title)
	options.Subtitle = SanitizeText(options.Subtitle)
	options.Empty = SanitizeText(options.Empty)
	options.Footer = SanitizeText(options.Footer)
	options.Initial = SanitizeText(options.Initial)
	if options.Output == nil {
		options.Output = os.Stderr
	}
	output := &selectorOutput{Writer: options.Output}
	options.Output = output
	defer func() {
		if outputErr := output.failure(); outputErr != nil {
			selection = Result{}
			err = selectorFailure{cause: errors.Join(err, outputErr)}
		}
	}()
	if options.Empty == "" {
		options.Empty = "No choices are available."
	}
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "type to filter"
	input.SetValue(options.Initial)
	input.Focus()
	model := chooserModel{options: options, choices: NewModel(options.Items, 8), input: input, width: 80, height: 24}
	model.choices.requireFilter = options.RequireFilter
	model.choices.SetFilter(options.Initial)
	refreshParent := options.Context
	if refreshParent == nil {
		refreshParent = context.Background()
	}
	refreshContext, cancelRefresh := context.WithCancel(refreshParent)
	defer cancelRefresh()
	model.options.Context = refreshContext
	programOptions := ProgramOptions(options.Stdin, options.Output)
	program := tea.NewProgram(model, programOptions...)
	final, runErr := program.Run()
	if runErr != nil {
		return Result{}, programFailure(options.Context, runErr)
	}
	if options.Context != nil && options.Context.Err() != nil {
		return Result{}, programFailure(options.Context, options.Context.Err())
	}
	result := final.(chooserModel)
	if result.interrupted {
		return Result{}, ErrInterrupted
	}
	if result.canceled || !result.confirmed {
		return Result{}, ErrCanceled
	}
	return Result{Item: result.selected, Action: result.action}, nil
}

type loadDoneMsg struct{ err error }

type loadingModel struct {
	ctx           context.Context
	title, detail string
	work          func(context.Context) error
	cancel        context.CancelFunc
	done          bool
	interrupted   bool
	err           error
	spin          spinner.Model
	width, height int
}

func (m loadingModel) Init() tea.Cmd {
	return tea.Batch(RequestBackground(), func() tea.Msg { return loadDoneMsg{err: m.work(context.Background())} }, m.spin.Tick)
}

func (m loadingModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = viewWidth(message.Width), viewHeight(message.Height)
	case tea.BackgroundColorMsg:
		ObserveBackground(message)
	case tea.KeyPressMsg:
		if (message.String() == "ctrl+c" || KeyMatches(m.ctx, "back", message.String())) && !m.interrupted {
			m.interrupted = true
			m.cancel()
			m.detail = "Canceling"
			return m, nil
		}
	case loadDoneMsg:
		m.done, m.err = true, message.err
		return m, tea.Quit
	case spinner.TickMsg:
		var command tea.Cmd
		m.spin, command = m.spin.Update(message)
		return m, command
	}
	return m, nil
}

func (m loadingModel) View() tea.View { return NewView(m.ctx, m.render()) }

func (m loadingModel) render() string {
	lineWidth := viewWidth(m.width)
	height := viewHeight(m.height)
	styles := stylesForContext(m.ctx)
	boat := []string{"      ▄█▄", "  ▄▄▝▀▀▀▀▀▘▄▄", "   ▀███████▀"}
	lines := []string{styles.title.Render(truncateLine(SanitizeText(m.title), lineWidth)), ""}
	for _, line := range boat {
		lines = append(lines, truncateLine(line, lineWidth))
	}
	lines = append(lines, "", truncateLine("  "+m.spin.View()+"  "+SanitizeText(m.detail), lineWidth))
	if height == 1 {
		return styles.help.Render(loadingFooter(m.ctx, lineWidth))
	}
	lines = lines[:min(len(lines), height-1)]
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, styles.help.Render(loadingFooter(m.ctx, lineWidth)))
	return strings.Join(lines, "\n")
}

// Loading runs work while an animated state remains visible in the current TUI.
// Work must honor cancellation and return after releasing its owned resources.
func Loading(ctx context.Context, title, detail string, input *os.File, output io.Writer, work func(context.Context) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if input == nil {
		input = os.Stdin
	}
	if output == nil {
		output = os.Stderr
	}
	fenced := &selectorOutput{Writer: output}
	output = fenced
	defer func() {
		if outputErr := fenced.failure(); outputErr != nil {
			err = selectorFailure{cause: errors.Join(err, outputErr)}
		}
	}()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone := make(chan struct{})
	var workerErr error
	go func() { defer close(workerDone); workerErr = loadingWork(workCtx, work) }()
	model := loadingModel{spin: spinner.New(spinner.WithSpinner(spinner.Line)), ctx: ctx, title: title, detail: detail, width: 80, height: 24, cancel: cancel, work: func(context.Context) error { <-workerDone; return workerErr }}
	programOptions := ProgramOptions(input, output)
	if ScreenActive() {
		writeScreen(output, model.render()+"\x1b[H")
	}
	final, runErr := tea.NewProgram(model, programOptions...).Run()
	cancel()
	<-workerDone
	if runErr != nil {
		return programFailure(ctx, errors.Join(runErr, workerErr))
	}
	if ctx.Err() != nil {
		return programFailure(ctx, errors.Join(ctx.Err(), workerErr))
	}
	result := final.(loadingModel)
	if result.interrupted {
		cause := context.Cause(workCtx)
		if workerErr == nil || (cancellationOnly(workerErr, context.Canceled, false) && cancellationOnly(cause, context.Canceled, false)) {
			return ErrCanceled
		}
		return selectorFailure{cause: errors.Join(ErrCanceled, workerErr, cause)}
	}
	if result.err != nil {
		return selectorFailure{cause: result.err}
	}
	return nil
}

type chooserModel struct {
	options       Options
	choices       *Model
	input         textinput.Model
	width         int
	height        int
	selected      Item
	confirmed     bool
	action        string
	canceled      bool
	interrupted   bool
	refreshFailed bool
	lastClickRow  int // 1-based visible row of the last click, 0 for none
	lastClickAt   time.Time
	filterSeq     int  // latest scheduled debounced filter
	filterPending bool // the input changed but the filter has not run yet
}

// Lists longer than debounceAbove filter 50 ms after the last keystroke; shorter ones
// filter on every keystroke (well under a frame).
const (
	debounceAbove  = 500
	filterDebounce = 50 * time.Millisecond
)

type filterDebounced struct{ seq int }

// flushFilter runs a pending debounced filter before anything acts on the list.
func (m *chooserModel) flushFilter() {
	if m.filterPending {
		m.choices.SetFilter(m.input.Value())
		m.filterPending = false
	}
}

const doubleClickWindow = 500 * time.Millisecond

type chooserCanceled struct{}

type choicesRefreshTick struct{}
type choicesRefreshed struct {
	items []Item
	err   error
}

func (m chooserModel) refreshTick() tea.Cmd {
	if m.options.Refresh == nil {
		return nil
	}
	return func() tea.Msg {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return choicesRefreshTick{}
		case <-m.options.Context.Done():
			return nil
		}
	}
}

func (m chooserModel) Init() tea.Cmd {
	var cancellation tea.Cmd
	if m.options.Context != nil {
		cancellation = func() tea.Msg { <-m.options.Context.Done(); return chooserCanceled{} }
	}
	return tea.Batch(RequestBackground(), textinput.Blink, m.refreshTick(), cancellation)
}

func (m chooserModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message.(type) {
	case tea.PasteMsg, tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseWheelMsg:
		if m.tooSmall() {
			return m, nil
		}
	}
	switch message := message.(type) {
	case chooserCanceled:
		return m, tea.Quit
	case filterDebounced:
		if message.seq == m.filterSeq {
			m.flushFilter()
		}
		return m, nil
	case choicesRefreshTick:
		if m.options.Refresh == nil {
			return m, nil
		}
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.options.Context, 4*time.Second)
			defer cancel()
			var items []Item
			err := loadingWork(ctx, func(ctx context.Context) error {
				var refreshErr error
				items, refreshErr = m.options.Refresh(ctx)
				return refreshErr
			})
			return choicesRefreshed{items: items, err: err}
		}
	case choicesRefreshed:
		if message.err == nil {
			m.refreshFailed = false
			selected, _ := m.choices.Selected()
			m.choices.SetItems(message.items)
			m.choices.selected, m.choices.offset = 0, 0
			for position, index := range m.choices.visible {
				if m.choices.items[index].ID == selected.ID {
					m.choices.selected = position
					break
				}
			}
			m.choices.ensureVisible()
			m.options.Subtitle = strings.TrimSuffix(m.options.Subtitle, " · refresh failed; showing last observation")
		} else {
			if !m.refreshFailed {
				observeFailure(m.options.Context, message.err)
				m.refreshFailed = true
			}
			if !strings.HasSuffix(m.options.Subtitle, " · refresh failed; showing last observation") {
				m.options.Subtitle += " · refresh failed; showing last observation"
			}
		}
		return m, m.refreshTick()
	case tea.BackgroundColorMsg:
		ObserveBackground(message)
	case tea.WindowSizeMsg:
		if message.Width <= 0 || message.Height <= 0 {
			break // the terminal reports no size (some ptys): keep the last known one
		}
		m.width, m.height = viewWidth(message.Width), viewHeight(message.Height)
		m.input.SetWidth(filterInputWidth(m.width, m.options.RequireFilter))
		bodyLines := headerLineCount(m.options.Header) + 2
		if m.options.Subtitle != "" {
			bodyLines++
		}
		m.choices.rows = max(1, (m.height-2-bodyLines)/itemRowHeight(m.options.Context))
		m.choices.ensureVisible()
	case tea.KeyPressMsg:
		key := message.String()
		// Ctrl+C remains an unconditional interrupt even when a caller or a
		// preference maps another action to the same key.
		if key == "ctrl+c" {
			m.interrupted = true
			return m, tea.Quit
		}
		if KeyMatches(m.options.Context, "back", key) {
			m.canceled = true
			return m, tea.Quit
		}
		if m.tooSmall() {
			return m, nil // never select or type into a menu that cannot be read
		}
		if KeyMatches(m.options.Context, "select", key) {
			m.flushFilter()
			if item, ok := m.choices.Selected(); ok {
				m.selected, m.confirmed = item, true
				return m, tea.Quit
			}
		}
		if action, ok := m.options.Actions[key]; ok {
			m.flushFilter()
			m.action = action
			m.selected, _ = m.choices.Selected()
			m.confirmed = true
			return m, tea.Quit
		}
		if KeyMatches(m.options.Context, "up", key) {
			m.flushFilter()
			m.choices.Move(-1)
			return m, nil
		}
		if KeyMatches(m.options.Context, "down", key) {
			m.flushFilter()
			m.choices.Move(1)
			return m, nil
		}
		return m.typed(message)
	case tea.PasteMsg:
		return m.typed(message)
	case tea.MouseClickMsg:
		if message.Button == tea.MouseLeft {
			if action, ok := m.options.HeaderActions[message.Y]; ok {
				m.action, m.confirmed = action, true
				return m, tea.Quit
			}
			index, ok := m.itemAtRow(message.Y)
			if !ok {
				break
			}
			// A click moves the selection; only a second click on the same row
			// confirms, so a stray click never opens or runs anything.
			doubleClick := m.lastClickRow == index+1 && time.Since(m.lastClickAt) <= doubleClickWindow
			m.choices.selected = index
			m.lastClickRow, m.lastClickAt = index+1, time.Now()
			if item, selected := m.choices.Selected(); selected && doubleClick {
				m.selected, m.confirmed = item, true
				return m, tea.Quit
			}
		}
	case tea.MouseMotionMsg:
		if index, ok := m.itemAtRow(message.Y); ok {
			m.choices.selected = index
			m.choices.ensureVisible()
		}
	case tea.MouseWheelMsg:
		switch message.Button {
		case tea.MouseWheelUp:
			m.choices.Move(-1)
		case tea.MouseWheelDown:
			m.choices.Move(1)
		}
	}
	return m, nil
}

// typed feeds typed or pasted text to the filter input.
func (m chooserModel) typed(message tea.Msg) (tea.Model, tea.Cmd) {
	var command tea.Cmd
	m.input, command = m.input.Update(message)
	value := SanitizeText(m.input.Value())
	if value != m.input.Value() {
		m.input.SetValue(value)
	}
	if len(m.choices.items) > debounceAbove {
		m.filterSeq++
		m.filterPending = true
		seq := m.filterSeq
		command = tea.Batch(command, tea.Tick(filterDebounce, func(time.Time) tea.Msg { return filterDebounced{seq} }))
	} else {
		m.choices.SetFilter(value)
	}
	if m.options.InputSelection != nil {
		if selected, ok := m.options.InputSelection(value); ok {
			m.selected, m.confirmed = selected, true
			return m, tea.Quit
		}
	}
	return m, command
}

func (m chooserModel) itemAtRow(row int) (int, bool) {
	first := headerLineCount(m.options.Header) + 2
	if m.options.Subtitle != "" {
		first++
	}
	if row < first {
		return 0, false
	}
	rowHeight := itemRowHeight(m.options.Context)
	index := m.choices.offset + (row-first)/rowHeight
	end := min(len(m.choices.visible), m.choices.offset+m.choices.rows)
	return index, index >= m.choices.offset && index < end
}

func (m chooserModel) View() tea.View { return NewView(m.options.Context, m.render()) }

func (m chooserModel) render() string {
	lineWidth := viewWidth(m.width)
	height := viewHeight(m.height)
	styles := stylesForContext(m.options.Context)
	if m.tooSmall() {
		return styles.help.Render(truncateLine("Enlarge terminal to use this menu; Esc goes back", lineWidth))
	}
	body := make([]string, 0, height)
	header := m.options.Header // sanitized once by ChooseWithAction
	if header != "" {
		for _, line := range strings.Split(header, "\n") {
			body = append(body, truncateLine(line, lineWidth))
		}
		body = append(body, "")
	}
	body = append(body, styles.title.Render(truncateLine(m.options.Title, lineWidth)))
	if subtitle := m.options.Subtitle; subtitle != "" {
		body = append(body, styles.subtitle.Render(truncateLine(subtitle, lineWidth)))
	}
	body = append(body, "")
	end := min(len(m.choices.visible), m.choices.offset+m.choices.rows)
	compact := isCompact(m.options.Context)
	for visibleIndex := m.choices.offset; visibleIndex < end; visibleIndex++ {
		item := m.choices.items[m.choices.visible[visibleIndex]]
		prefix := "     "
		if visibleIndex == m.choices.selected {
			prefix = "  >  "
		}
		plainTitle := prefix + item.Title
		if item.Favorite {
			plainTitle += " ◆"
		}
		title := truncateLine(plainTitle, lineWidth)
		detail := truncateLine("     "+item.Description, lineWidth)
		if visibleIndex == m.choices.selected {
			title = styles.selected.Render(title + strings.Repeat(" ", max(0, lineWidth-ansi.StringWidth(title))))
		} else if item.Action {
			title = styles.action.Render(title)
		} else if item.Favorite {
			name := truncateLine(prefix+item.Title, max(1, lineWidth-2))
			title = styles.favorite.Render(name) + " " + styles.favoriteMarker.Render("◆")
		}
		body = append(body, truncateLine(title, lineWidth))
		if !compact {
			body = append(body, styles.subtitle.Render(detail))
		}
	}
	if len(m.choices.visible) == 0 {
		if m.choices.requireFilter && m.choices.Filter() == "" {
			body = append(body, styles.subtitle.Render(truncateLine("  Start typing a filename or path to search.", lineWidth)), "")
		} else {
			empty := "No matches"
			if len(m.choices.items) == 0 {
				empty = m.options.Empty
			}
			body = append(body, styles.subtitle.Render(truncateLine("  "+empty, lineWidth)), "")
		}
	}
	footer := m.options.Footer
	if footer == "" {
		ctx := m.options.Context
		footer = Footer(lineWidth, Move(ctx, "move"), Binding(ctx, "select", "select"), Hint("type", "filter", "type"), Binding(ctx, "back", "back"), Binding(ctx, "interrupt", "interrupt"))
	}
	filterLabel := "Filter"
	if m.choices.requireFilter {
		filterLabel = "Search files"
	}
	filterLine := filterLabel + "  " + m.input.View()
	filterLine = truncateLine(filterLine, max(1, lineWidth-2))
	filterLine += strings.Repeat(" ", max(0, lineWidth-2-ansi.StringWidth(filterLine)))
	filter := filterLine
	if lineWidth >= 3 {
		filter = styles.filter.Render(filterLine)
	}
	bodyLimit := max(0, height-2)
	if len(body) > bodyLimit {
		body = body[:bodyLimit]
	}
	for len(body) < bodyLimit {
		body = append(body, "")
	}
	lines := append(body, filter, styles.help.Render(truncateLine(footer, lineWidth)))
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func viewWidth(width int) int {
	return max(1, width)
}

func viewHeight(height int) int {
	return max(1, height)
}

// minWidth and minHeight are the smallest terminal a menu can be read and used in.
const (
	minWidth  = 40
	minHeight = 5
)

func (m chooserModel) tooSmall() bool { return m.width < minWidth || m.height < minHeight }

func truncateLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	tail := "..."
	if width <= len(tail) {
		tail = "" // an ellipsis must never be wider than the screen
	}
	return ansi.Truncate(value, width, tail)
}

func filterInputWidth(width int, requireFilter bool) int {
	label := "Filter"
	if requireFilter {
		label = "Search files"
	}
	return max(1, width-2-ansi.StringWidth(label+"  ")-3) // textinput adds "> " and the cursor
}

func headerLineCount(header string) int {
	if header == "" {
		return 0
	}
	return strings.Count(header, "\n") + 2
}
