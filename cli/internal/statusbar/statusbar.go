// Package statusbar provides the small local status line used by interactive
// terminal sessions. It never writes bytes to the remote connection.
package statusbar

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"golang.org/x/term"
)

const (
	ModeAuto = "auto"
	ModeOn   = "on"
	ModeOff  = "off"

	FullscreenHide = "hide"
	FullscreenShow = "show"

	ThemeTerminal = "terminal"
	ThemeDark     = "dark"
	ThemeLight    = "light"
	ThemeMono     = "mono"
	minimumWidth  = 20
)

// Layout controls which widgets appear in each status-bar region.
type Layout struct {
	Left   []string
	Center []string
	Right  []string
}

// Colors optionally override colors supplied by a theme. Values are validated
// configuration inputs: ANSI color names, "default", or #RRGGBB.
type Colors struct {
	Foreground string
	Background string
	Accent     string
	Warning    string
	Error      string
}

// Options are deliberately small: the renderer only needs terminal
// capability, an output stream, and the configured notice duration.
type Options struct {
	Mode           string
	Fullscreen     string
	Theme          string
	Privacy        bool
	TerminalTitle  bool
	Colors         Colors
	NoticeDuration time.Duration
	Layout         Layout
	Output         *os.File
	Input          *os.File
	Term           string
	IsTerminal     func(int) bool
	GetSize        func(int) (int, int, error)
	// ViewportChanged is called when alternate-screen suspension changes the
	// number of rows available to the remote PTY.
	ViewportChanged func(cols, rows uint16)
}

// Bar serializes remote output with local redraws. It is also an io.Writer so
// session can give it sole ownership of stdout for the attached session.
type Bar struct {
	mu              sync.Mutex
	out             io.Writer
	serializedOut   io.Writer
	outputFD        int
	eligible        bool
	enabled         bool
	suspended       bool
	alternate       bool
	closed          bool
	lastRows        int
	scrollRows      int
	scrollDirty     bool
	noticeDuration  time.Duration
	project         string
	session         string
	connection      string
	transport       string
	localVersion    string
	remoteVersion   string
	credits         string
	storage         string
	configSync      string
	recording       bool // the team records this session: REC is always shown
	layout          Layout
	notice          string
	noticeUntil     time.Time
	loading         bool
	spinner         int
	failures        map[string]failure
	failureOrder    uint64
	timer           *time.Timer
	spinnerTimer    *time.Timer
	parser          *ansi.Parser
	ansiState       byte
	appCursorSaved  bool
	appInverse      bool
	appSGR          []byte
	appSGROverflow  bool
	synchronized    bool
	redrawPending   bool
	isTerminal      func(int) bool
	getSize         func(int) (int, int, error)
	viewportChanged func(cols, rows uint16)
	fullscreen      string
	theme           string
	privacy         bool
	colors          Colors
	noColor         bool
	terminalTitle   bool
	titlePushed     bool
	remoteWrites    chan outputWrite
	redraw          chan struct{}
	ownerStop       chan struct{}
	ownerDone       chan struct{}
	ownerStopOnce   sync.Once
}

type outputWrite struct {
	data   []byte
	result chan outputResult
}
type outputResult struct {
	n   int
	err error
}

type serializedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *serializedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

type failure struct {
	message string
	order   uint64
}

// New returns a bar which may be disabled when the local terminal cannot
// safely support cursor-addressed output. Mode on still respects capability
// checks, rather than corrupting redirected or dumb terminals.
// terminalName picks the terminal type the bar checks. cmd.exe and PowerShell never set TERM, but
// every Windows console that can run pb (Windows 10+) takes the VT sequences the bar draws with.
func terminalName(option, env, goos string) string {
	if name := strings.TrimSpace(option); name != "" {
		return name
	}
	if env == "" && goos == "windows" {
		return "xterm-256color"
	}
	return env
}

func New(options Options) *Bar {
	output := options.Output
	if output == nil {
		output = os.Stdout
	}
	input := options.Input
	if input == nil {
		input = os.Stdin
	}
	isTerminal := options.IsTerminal
	if isTerminal == nil {
		isTerminal = term.IsTerminal
	}
	getSize := options.GetSize
	if getSize == nil {
		getSize = term.GetSize
	}
	mode := strings.ToLower(strings.TrimSpace(options.Mode))
	if mode == "" {
		mode = ModeAuto
	}
	compatibleTerm := terminalName(options.Term, os.Getenv("TERM"), runtime.GOOS)
	eligible := mode != ModeOff && !strings.EqualFold(compatibleTerm, "dumb") && compatibleTerm != "" &&
		isTerminal(int(input.Fd())) && isTerminal(int(output.Fd()))
	if options.NoticeDuration <= 0 {
		options.NoticeDuration = 5 * time.Second
	}
	serializedOut := &serializedWriter{out: output}
	b := &Bar{
		out:             output,
		serializedOut:   serializedOut,
		outputFD:        int(output.Fd()),
		eligible:        eligible,
		noticeDuration:  options.NoticeDuration,
		connection:      "connecting",
		failures:        make(map[string]failure),
		parser:          ansi.NewParser(),
		ansiState:       byte(parser.GroundState),
		scrollDirty:     true,
		isTerminal:      isTerminal,
		getSize:         getSize,
		layout:          normalizeLayout(options.Layout),
		viewportChanged: options.ViewportChanged,
		fullscreen:      normalized(options.Fullscreen, FullscreenHide),
		theme:           normalized(options.Theme, ThemeTerminal),
		privacy:         options.Privacy,
		colors:          options.Colors,
		noColor:         os.Getenv("NO_COLOR") != "",
		terminalTitle:   options.TerminalTitle,
		remoteWrites:    make(chan outputWrite, 64),
		redraw:          make(chan struct{}, 1),
		ownerStop:       make(chan struct{}),
		ownerDone:       make(chan struct{}),
	}
	b.mu.Lock()
	b.refreshViewportLocked()
	b.drawNowLocked()
	b.mu.Unlock()
	go b.runOutputOwner()
	return b
}

// clearMarginBeforeFullscreen drops the bar's scroll margin before a chunk that enters the alternate screen is written: the
// chunk is often a whole screen checkpoint (rows separated by CRLF), and with the margin still set its last LF scrolls the
// top row away. consumeANSI clears it only after the chunk. shortcut: a sequence split across two writes is not seen.
func (b *Bar) clearMarginBeforeFullscreen(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.alternate && bytes.Contains(data, []byte("\x1b[?1049h")) {
		_, _ = fmt.Fprint(b.serializedOut, "\x1b[r")
	}
}

func (b *Bar) runOutputOwner() {
	defer close(b.ownerDone)
	var redrawTimer *time.Timer
	var redrawC <-chan time.Time
	for {
		select {
		case request := <-b.remoteWrites:
			b.clearMarginBeforeFullscreen(request.data)
			n, err := b.serializedOut.Write(request.data)
			if n > 0 {
				b.mu.Lock()
				invalidated := b.consumeANSI(request.data[:n])
				if !b.closed && b.ansiState == byte(parser.GroundState) && (invalidated || b.redrawPending) {
					b.drawLocked()
				}
				b.mu.Unlock()
			}
			request.result <- outputResult{n: n, err: err}
			continue
		default:
		}
		select {
		case request := <-b.remoteWrites:
			b.clearMarginBeforeFullscreen(request.data)
			n, err := b.serializedOut.Write(request.data)
			if n > 0 {
				b.mu.Lock()
				invalidated := b.consumeANSI(request.data[:n])
				if !b.closed && b.ansiState == byte(parser.GroundState) && (invalidated || b.redrawPending) {
					b.drawLocked()
				}
				b.mu.Unlock()
			}
			request.result <- outputResult{n: n, err: err}
		case <-b.redraw:
			if redrawTimer == nil {
				redrawTimer = time.NewTimer(time.Second / 60)
				redrawC = redrawTimer.C
			}
		case <-redrawC:
			redrawTimer = nil
			redrawC = nil
			b.mu.Lock()
			if !b.closed {
				b.drawNowLocked()
			}
			b.mu.Unlock()
		case <-b.ownerStop:
			return
		}
	}
}

func normalized(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func normalizeLayout(layout Layout) Layout {
	if layout.Left == nil {
		layout.Left = []string{"project", "session"}
	}
	if layout.Center == nil {
		layout.Center = []string{"activity"}
	}
	if layout.Right == nil {
		layout.Right = []string{"credits", "connection"}
	}
	return Layout{Left: normalizeWidgets(layout.Left), Center: normalizeWidgets(layout.Center), Right: normalizeWidgets(layout.Right)}
}

func normalizeWidgets(widgets []string) []string {
	result := make([]string, 0, len(widgets))
	for _, widget := range widgets {
		widget = strings.ToLower(strings.TrimSpace(widget))
		if widget != "" {
			result = append(result, widget)
		}
	}
	return result
}

func (b *Bar) Enabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.refreshViewportLocked()
	return b.enabled
}

// Write streams remote bytes unchanged, then redraws only after a complete
// ANSI sequence. That prevents the cursor movement used for the local line
// from being inserted in the middle of a split escape sequence.
func (b *Bar) Write(p []byte) (int, error) {
	if !b.eligible {
		return b.out.Write(p)
	}
	request := outputWrite{data: append([]byte(nil), p...), result: make(chan outputResult, 1)}
	select {
	case b.remoteWrites <- request:
	case <-b.ownerDone:
		return 0, io.ErrClosedPipe
	}
	select {
	case result := <-request.result:
		return result.n, result.err
	case <-b.ownerDone:
		return 0, io.ErrClosedPipe
	}
}

func (b *Bar) activeLocked() bool { return b.enabled && !b.suspended }

func (b *Bar) currentFailureLocked() string {
	var current failure
	for _, candidate := range b.failures {
		if candidate.order > current.order {
			current = candidate
		}
	}
	return current.message
}

func (b *Bar) clearLocked() {
	if b.lastRows < 1 {
		return
	}
	_, _ = fmt.Fprintf(b.serializedOut, "\x1b[s\x1b[%d;1H\x1b[2K\x1b[r\x1b[u", b.lastRows)
	b.scrollRows = 0
	b.scrollDirty = true
}

// Close clears the reserved row. It is safe to call more than once.
func (b *Bar) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.ownerStopOnce.Do(func() { close(b.ownerStop) })
		<-b.ownerDone
		return nil
	}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.stopSpinnerLocked()
	b.enabled = false
	b.closed = true
	b.mu.Unlock()
	b.ownerStopOnce.Do(func() { close(b.ownerStop) })
	<-b.ownerDone
	b.mu.Lock()
	b.flushPendingRedrawLocked()
	b.clearLocked()
	b.restoreTitleLocked()
	b.enabled = false
	b.mu.Unlock()
	return nil
}
