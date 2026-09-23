// Package selector provides Paperboat's shared interactive list selection UI.
package selector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
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
		_, _ = io.WriteString(output, enterAlternateScreen)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if persistentScreen.Add(-1) == 0 {
				_, _ = io.WriteString(output, leaveAlternateScreen)
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
		_, _ = io.WriteString(output, leaveAlternateScreen)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if suspendedScreen.Add(-1) == 0 && persistentScreen.Load() > 0 {
				_, _ = io.WriteString(output, enterAlternateScreen)
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
	options := []tea.ProgramOption{tea.WithInput(input), tea.WithOutput(output), tea.WithMouseAllMotion()}
	if persistentScreen.Load() == 0 || suspendedScreen.Load() > 0 {
		return append(options, tea.WithAltScreen())
	}
	_, _ = io.WriteString(output, "\x1b[2J\x1b[H")
	return options
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

func NewModel(items []Item, rows int) *Model {
	searchable := make([]string, len(items))
	primary := make([]string, len(items))
	searchChars := make([]util.Chars, len(items))
	for index, item := range items {
		primary[index] = strings.ToLower(item.Title)
		searchable[index] = strings.ToLower(strings.TrimSpace(strings.Join([]string{item.Title, item.Search, item.Description}, " ")))
		searchChars[index] = util.ToChars([]byte(searchable[index]))
	}
	workerCount := min(max(1, runtime.GOMAXPROCS(0)), 8, max(1, len(items)))
	workerMatches := make([][]rankedMatch, workerCount)
	workerSlabs := make([]*util.Slab, workerCount)
	for index := range workerCount {
		workerMatches[index] = make([]rankedMatch, 0, (len(items)+workerCount-1)/workerCount)
		workerSlabs[index] = util.MakeSlab(100*1024, 2048)
	}
	m := &Model{
		items: items, primary: primary, searchable: searchable, searchChars: searchChars,
		matches: make([]rankedMatch, 0, len(items)), workerMatches: workerMatches, workerSlabs: workerSlabs,
		rows: max(1, rows),
	}
	m.applyFilter()
	return m
}

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
		var workers sync.WaitGroup
		for workerIndex := range m.workerMatches {
			workers.Add(1)
			go func(workerIndex int) {
				defer workers.Done()
				matches := m.workerMatches[workerIndex][:0]
				for index := workerIndex; index < len(m.searchChars); index += len(m.workerMatches) {
					result, _ := algo.FuzzyMatchV2(false, false, true, &m.searchChars[index], pattern, false, m.workerSlabs[workerIndex])
					if result.Start >= 0 {
						matches = append(matches, rankedMatch{index: index, class: matchClass(m.primary[index], query), score: result.Score, start: result.Start, length: len(m.searchable[index])})
					}
				}
				m.workerMatches[workerIndex] = matches
			}(workerIndex)
		}
		workers.Wait()
		for _, matches := range m.workerMatches {
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
	items := make([]Item, len(options.Items))
	copy(items, options.Items)
	for index := range items {
		items[index].Title = SanitizeText(items[index].Title)
		items[index].Description = SanitizeText(items[index].Description)
		items[index].Search = SanitizeText(items[index].Search)
	}
	options.Items = items
	if options.Output == nil {
		options.Output = os.Stderr
	}
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
	programOptions := ProgramOptions(options.Stdin, options.Output)
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	program := tea.NewProgram(model, programOptions...)
	final, runErr := program.Run()
	if runErr != nil {
		if options.Context != nil && options.Context.Err() != nil {
			return Result{}, options.Context.Err()
		}
		return Result{}, fmt.Errorf("run selector: %w", runErr)
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
type loadTickMsg struct{}

type loadingModel struct {
	ctx           context.Context
	title, detail string
	work          func(context.Context) error
	cancel        context.CancelFunc
	done          bool
	interrupted   bool
	err           error
	frame         int
	width, height int
}

func (m loadingModel) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg { return loadDoneMsg{err: m.work(context.Background())} }, loadingTick())
}

func loadingTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return loadTickMsg{} })
}

func (m loadingModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = viewWidth(message.Width), viewHeight(message.Height)
	case tea.KeyMsg:
		if (message.String() == "ctrl+c" || KeyMatches(m.ctx, "back", message.String())) && !m.interrupted {
			m.interrupted = true
			m.cancel()
			m.detail = "Canceling"
			return m, nil
		}
	case loadDoneMsg:
		m.done, m.err = true, message.err
		return m, tea.Quit
	case loadTickMsg:
		m.frame = (m.frame + 1) % 4
		return m, loadingTick()
	}
	return m, nil
}

func (m loadingModel) View() string {
	lineWidth := viewWidth(m.width)
	height := viewHeight(m.height)
	styles := stylesForContext(m.ctx)
	boat := []string{"      ▄█▄", "  ▄▄▝▀▀▀▀▀▘▄▄", "   ▀███████▀"}
	lines := []string{styles.title.Render(truncateLine(SanitizeText(m.title), lineWidth)), ""}
	for _, line := range boat {
		lines = append(lines, truncateLine(line, lineWidth))
	}
	lines = append(lines, "", truncateLine(fmt.Sprintf("  %c  %s", "|/-\\"[m.frame], SanitizeText(m.detail)), lineWidth))
	if height == 1 {
		return styles.help.Render(truncateLine(loadingFooter(m.ctx), lineWidth))
	}
	lines = lines[:min(len(lines), height-1)]
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, styles.help.Render(truncateLine(loadingFooter(m.ctx), lineWidth)))
	return strings.Join(lines, "\n")
}

// Loading runs work while an animated state remains visible in the current TUI.
func Loading(ctx context.Context, title, detail string, input *os.File, output io.Writer, work func(context.Context) error) error {
	if input == nil {
		input = os.Stdin
	}
	if output == nil {
		output = os.Stderr
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := loadingModel{ctx: ctx, title: title, detail: detail, width: 80, height: 24, cancel: cancel, work: func(context.Context) error { return work(workCtx) }}
	programOptions := ProgramOptions(input, output)
	if ScreenActive() {
		_, _ = io.WriteString(output, model.View()+"\x1b[H")
	}
	final, err := tea.NewProgram(model, programOptions...).Run()
	if err != nil {
		return fmt.Errorf("run loading state: %w", err)
	}
	result := final.(loadingModel)
	if result.interrupted {
		return ErrCanceled
	}
	return result.err
}

type chooserModel struct {
	options     Options
	choices     *Model
	input       textinput.Model
	width       int
	height      int
	selected    Item
	confirmed   bool
	action      string
	canceled    bool
	interrupted bool
}

func (m chooserModel) Init() tea.Cmd { return textinput.Blink }

func (m chooserModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = viewWidth(message.Width), viewHeight(message.Height)
		m.input.Width = filterInputWidth(m.width, m.options.RequireFilter)
		bodyLines := headerLineCount(sanitizeHeader(m.options.Header)) + 2
		if m.options.Subtitle != "" {
			bodyLines++
		}
		m.choices.rows = max(1, (m.height-2-bodyLines)/itemRowHeight(m.options.Context))
		m.choices.ensureVisible()
	case tea.KeyMsg:
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
		if KeyMatches(m.options.Context, "select", key) {
			if item, ok := m.choices.Selected(); ok {
				m.selected, m.confirmed = item, true
				return m, tea.Quit
			}
		}
		if action, ok := m.options.Actions[key]; ok {
			m.action = action
			m.selected, _ = m.choices.Selected()
			m.confirmed = true
			return m, tea.Quit
		}
		switch message.String() {
		case "up", "ctrl+k":
			m.choices.Move(-1)
			return m, nil
		case "down", "ctrl+n":
			m.choices.Move(1)
			return m, nil
		}
		if KeyMatches(m.options.Context, "up", key) {
			m.choices.Move(-1)
			return m, nil
		}
		if KeyMatches(m.options.Context, "down", key) {
			m.choices.Move(1)
			return m, nil
		}
		var command tea.Cmd
		m.input, command = m.input.Update(message)
		value := SanitizeText(m.input.Value())
		if value != m.input.Value() {
			m.input.SetValue(value)
		}
		m.choices.SetFilter(value)
		if m.options.InputSelection != nil {
			if selected, ok := m.options.InputSelection(value); ok {
				m.selected, m.confirmed = selected, true
				return m, tea.Quit
			}
		}
		return m, command
	case tea.MouseMsg:
		if message.Action == tea.MouseActionPress && message.Button == tea.MouseButtonLeft {
			if action, ok := m.options.HeaderActions[message.Y]; ok {
				m.action, m.confirmed = action, true
				return m, tea.Quit
			}
		}
		if message.Action == tea.MouseActionMotion {
			if index, ok := m.itemAtRow(message.Y); ok {
				m.choices.selected = index
				m.choices.ensureVisible()
			}
			break
		}
		switch message.Button {
		case tea.MouseButtonWheelUp:
			m.choices.Move(-1)
		case tea.MouseButtonWheelDown:
			m.choices.Move(1)
		case tea.MouseButtonLeft:
			if message.Action != tea.MouseActionPress {
				break
			}
			index, ok := m.itemAtRow(message.Y)
			if !ok {
				break
			}
			m.choices.selected = index
			if item, selected := m.choices.Selected(); selected {
				m.selected, m.confirmed = item, true
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m chooserModel) itemAtRow(row int) (int, bool) {
	first := headerLineCount(sanitizeHeader(m.options.Header)) + 2
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

var (
	brandColor     = lipgloss.AdaptiveColor{Light: "#1447E6", Dark: "#6F8CFF"}
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(brandColor)
	subtitleStyle  = lipgloss.NewStyle().Faint(true)
	selectedStyle  = lipgloss.NewStyle().Reverse(true)
	actionStyle    = lipgloss.NewStyle().Bold(true).Foreground(brandColor)
	favoriteStyle  = lipgloss.NewStyle().Bold(true).Foreground(brandColor)
	favoriteMarker = lipgloss.NewStyle().Bold(true).Foreground(brandColor)
	helpStyle      = lipgloss.NewStyle().Faint(true)
	filterStyle    = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15")).Padding(0, 1)
)

func (m chooserModel) View() string {
	lineWidth := viewWidth(m.width)
	height := viewHeight(m.height)
	styles := stylesForContext(m.options.Context)
	body := make([]string, 0, height)
	header := sanitizeHeader(m.options.Header)
	if header != "" {
		for _, line := range strings.Split(header, "\n") {
			body = append(body, truncateLine(line, lineWidth))
		}
		body = append(body, "")
	}
	body = append(body, styles.title.Render(truncateLine(SanitizeText(m.options.Title), lineWidth)))
	if subtitle := SanitizeText(m.options.Subtitle); subtitle != "" {
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
		plainTitle := prefix + SanitizeText(item.Title)
		if item.Favorite {
			plainTitle += " ◆"
		}
		title := truncateLine(plainTitle, lineWidth)
		detail := truncateLine("     "+SanitizeText(item.Description), lineWidth)
		if visibleIndex == m.choices.selected {
			title = styles.selected.Render(title + strings.Repeat(" ", max(0, lineWidth-ansi.StringWidth(title))))
		} else if item.Action {
			title = styles.action.Render(title)
		} else if item.Favorite {
			name := truncateLine(prefix+SanitizeText(item.Title), max(1, lineWidth-2))
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
			body = append(body, styles.subtitle.Render(truncateLine("  "+SanitizeText(empty), lineWidth)), "")
		}
	}
	footer := m.options.Footer
	if footer == "" {
		keys := HelpKeys(m.options.Context)
		footer = fmt.Sprintf("%s/%s move  %s/click select  backspace filter  %s back  %s interrupt", keys["up"], keys["down"], keys["select"], keys["back"], keys["interrupt"])
	}
	filterLabel := "Filter"
	if m.choices.requireFilter {
		filterLabel = "Search files"
	}
	input := m.input
	input.Width = filterInputWidth(lineWidth, m.options.RequireFilter)
	input.SetValue(SanitizeText(input.Value()))
	filterLine := filterLabel + "  " + input.View()
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
	lines := append(body, filter, styles.help.Render(truncateLine(SanitizeText(footer), lineWidth)))
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

func truncateLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, "...")
}

func filterInputWidth(width int, requireFilter bool) int {
	label := "Filter"
	if requireFilter {
		label = "Search files"
	}
	return max(1, width-2-ansi.StringWidth(label+"  "))
}

func headerLineCount(header string) int {
	if header == "" {
		return 0
	}
	return strings.Count(header, "\n") + 2
}
