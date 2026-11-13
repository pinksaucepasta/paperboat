//go:build windows

package pty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ErrInvalidCommand    = errors.New("invalid PTY command")
	ErrInvalidCWD        = errors.New("invalid PTY cwd")
	ErrInvalidDimensions = errors.New("invalid PTY dimensions")
	ErrInvalidSignal     = errors.New("invalid PTY signal")
)

type Dimensions struct {
	Columns uint16 `json:"columns"`
	Rows    uint16 `json:"rows"`
}
type Command struct {
	Path       string
	Args       []string
	Env        []string
	CWD        string
	Dimensions Dimensions
}
type ExitResult struct {
	Code     int       `json:"code"`
	Signal   string    `json:"signal,omitempty"`
	ExitedAt time.Time `json:"exited_at"`
}
type Signal string

const (
	Interrupt Signal = "SIGINT"
	Terminate Signal = "SIGTERM"
	Hangup    Signal = "SIGHUP"
	Kill      Signal = "SIGKILL"
)

type Adapter struct {
	root             string
	shellDirectories bool
}

func NewAdapter(root string) (*Adapter, error) {
	resolved, err := resolveDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("root: %w", ErrInvalidCWD)
	}
	return &Adapter{root: resolved}, nil
}

// NewShellAdapter allows managed interactive shells to start in any existing
// absolute directory accessible to the enrolled OS user. NewAdapter retains
// workspace confinement for exec and other scoped consumers.
func NewShellAdapter(root string) (*Adapter, error) {
	adapter, err := NewAdapter(root)
	if err != nil {
		return nil, err
	}
	adapter.shellDirectories = true
	return adapter, nil
}

func (a *Adapter) Start(command Command) (*Process, error) {
	path, err := ValidateProcessPolicy(command.Path, command.Args, command.Env)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveDirectory(command.CWD)
	if err != nil || !filepath.IsAbs(command.CWD) || !a.shellDirectories && !within(a.root, cwd) {
		return nil, ErrInvalidCWD
	}
	if !validDimensions(command.Dimensions) {
		return nil, ErrInvalidCommand
	}
	inputRead, inputWrite, err := anonymousPipe(true)
	if err != nil {
		return nil, fmt.Errorf("create ConPTY input pipe: %w", err)
	}
	outputRead, outputWrite, err := anonymousPipe(false)
	if err != nil {
		closeHandles(inputRead, inputWrite)
		return nil, fmt.Errorf("create ConPTY output pipe: %w", err)
	}
	console := windows.Handle(0)
	coord := windows.Coord{X: int16(command.Dimensions.Columns), Y: int16(command.Dimensions.Rows)}
	if err := windows.CreatePseudoConsole(coord, inputRead, outputWrite, 0, &console); err != nil {
		closeHandles(inputRead, inputWrite, outputRead, outputWrite)
		return nil, fmt.Errorf("create ConPTY: %w", err)
	}
	// ConPTY owns the console-side pipe handles after creation. The host keeps
	// only the write side of input and read side of output.
	windows.Close(inputRead)
	windows.Close(outputWrite)

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("create PTY job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("configure PTY job: %w", err)
	}

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("create process attributes: %w", err)
	}
	defer attributes.Delete()
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE takes the HPCON handle value,
	// represented as a pointer-sized value. Passing &console attaches a pointer
	// to the local variable instead and creates a child that never exchanges
	// input or output with the pseudoconsole. The handle is reinterpreted in place because converting
	// the uintptr with unsafe.Pointer(console) is reported by go vet.
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&console)), unsafe.Sizeof(console)); err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("configure process ConPTY: %w", err)
	}
	commandLine, err := commandLine(path, command.Args)
	if err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, err
	}
	workingDirectory, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, err
	}
	environment, err := environmentBlock(command.Env)
	if err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, err
	}
	startup := windows.StartupInfoEx{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	// ConPTY owns the child's console streams. Explicit null standard handles
	// prevent Windows from implicitly copying the parent's redirected streams;
	// enrolled daemon workers use NUL streams, which otherwise make shells exit
	// immediately instead of reading their pseudoconsole. This changes only the
	// child's startup record, never the concurrent host process's handles.
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.ProcThreadAttributeList = attributes.List()
	var processInfo windows.ProcessInformation
	// CREATE_NEW_PROCESS_GROUP disables Ctrl+C handling for the new process.
	// ConPTY sessions need terminal ETX input to reach the foreground command,
	// so process-tree ownership comes from the Job Object instead.
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED | windows.EXTENDED_STARTUPINFO_PRESENT)
	// The host runs in its own process group (service_entry_windows.go), which marks it as ignoring Ctrl+C, and the
	// flag is inherited by every child: a shell started here would ignore the CTRL_C_EVENT conhost makes of a typed
	// ETX, so only the cmd.exe fallback kill could stop a running command, and it took PowerShell down with it
	// (measured: spark/ctrlc-flag-product.md). Clear it for the child; a failure keeps the old behaviour.
	_ = setIgnoreCtrlC(false)
	// user-console: the user's ConPTY session child needs its console attached to the pseudoconsole.
	if err := windows.CreateProcess(nil, &commandLine[0], nil, nil, false, flags, environment, workingDirectory, &startup.StartupInfo, &processInfo); err != nil {
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("start ConPTY process: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		windows.Close(processInfo.Process)
		windows.Close(processInfo.Thread)
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("assign process to PTY job: %w", err)
	}
	if _, err := windows.ResumeThread(processInfo.Thread); err != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		windows.Close(processInfo.Process)
		windows.Close(processInfo.Thread)
		windows.Close(job)
		windows.ClosePseudoConsole(console)
		closeHandles(inputWrite, outputRead)
		return nil, fmt.Errorf("resume ConPTY process: %w", err)
	}
	windows.Close(processInfo.Thread)
	process := &Process{
		input:   os.NewFile(uintptr(inputWrite), "paperboat-conpty-input"),
		output:  os.NewFile(uintptr(outputRead), "paperboat-conpty-output"),
		console: console, process: processInfo.Process, processID: processInfo.ProcessId, job: job, done: make(chan struct{}),
	}
	go process.wait()
	return process, nil
}

type Process struct {
	writeMu     sync.Mutex
	interrupts  atomic.Int64 // bumped by every Interrupt: a write cut by it ends in ErrInterrupted, like the unix twin
	input       *os.File
	output      *os.File
	console     windows.Handle
	process     windows.Handle
	processID   uint32
	job         windows.Handle
	done        chan struct{}
	mu          sync.RWMutex
	result      ExitResult
	waitErr     error
	consoleOnce sync.Once
	closeOnce   sync.Once
	closeErr    error
	inputClosed bool // set under mu by CloseIO before the input handle closes
}

func (p *Process) Read(buffer []byte) (int, error) { return p.output.Read(buffer) }
func (p *Process) Write(data []byte) (int, error)  { return p.WriteContext(context.Background(), data) }
func (p *Process) WriteContext(ctx context.Context, data []byte) (int, error) {
	if err := LockInput(ctx, &p.writeMu); err != nil {
		return 0, err
	}
	defer p.writeMu.Unlock()
	start := p.interrupts.Load()
	n, err := writeInput(p.input, data, inputTimeout(ctx))
	if err != nil && p.interrupts.Load() != start {
		return n, ErrInterrupted
	}
	return n, err
}

// consoleModeQueryTimeout bounds the helper that reads the console mode before the key is acted on: it starts inside the
// session's pseudoconsole, and under an output flood it took over a second (X-24: the cut and the screen came 1.4 s after
// the key), where it takes 10-40 ms otherwise. A timeout is the unknown answer, which keeps the key an interrupt.
const consoleModeQueryTimeout = 200 * time.Millisecond

// Interrupt delivers a lone Ctrl-C (ETX) ahead of input stuck in a full ConPTY input pipe: a paste the foreground
// program never reads (a flood such as `yes`) blocks WriteFile, and the key would wait behind it for the 20 s input
// timeout, holding writeMu (the unix twin aborts the pending write by deadline, pty_unix.go Interrupt). CancelIoEx
// cancels the pending write from this thread (again every 50 ms while writeMu is taken, for a write that starts after
// the first cancel), the cut write ends in ErrInterrupted so the journal finalizes it instead of resending it, writeMu
// frees, and ETX, which ConPTY turns into the foreground Ctrl-C event, goes next. accepted runs first so the caller
// marks the input queued before key as discarded. Another key is written normally by the caller (false).
// Limit (measured): ConPTY has no input flush, so ETX still queues behind bytes conhost already accepted and the
// shell sees it once it reads them; the cut write itself ends at once and is final.
func (p *Process) Interrupt(key byte, accepted func()) (bool, error) {
	if key != 0x03 {
		return false, nil
	}
	// Like the unix twin (ISIG + VINTR): a program that put the console in raw mode (ENABLE_PROCESSED_INPUT clear: a
	// vim-like editor, PSReadLine mid-edit) gets 0x03 as data, so nothing is cut or discarded and the caller writes it
	// normally. Unknown (no console, helper failed or timed out) keeps the key an interrupt, as before.
	if p.console != 0 {
		if processed, err := p.inputIsProcessed(consoleModeQueryTimeout); err == nil && !processed {
			return false, nil
		}
	}
	if accepted != nil {
		accepted()
	}
	p.interrupts.Add(1)
	foreground := p.foregroundTree() // taken before the key, so a program the shell starts after it is never in the set
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			p.cancelInput()
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	err := LockInput(ctx, &p.writeMu)
	close(stop)
	<-stopped // no cancel may land on the key written below
	if err != nil {
		return false, err
	}
	_, err = writeInput(p.input, []byte{key}, inputTimeout(context.Background()))
	p.writeMu.Unlock() // the grace wait below must not hold up other writers
	if err != nil {
		return true, err
	}
	p.terminateIfStillRunning(foreground) // like Signal(Interrupt): the ETX alone does not stop every foreground program
	return true, nil
}

// cancelInput cancels the pending write on the input pipe; a closed pipe has nothing to cancel.
func (p *Process) cancelInput() {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.inputClosed {
		return
	}
	_ = windows.CancelIoEx(windows.Handle(p.input.Fd()), nil)
}

func (p *Process) Resize(dimensions Dimensions) error {
	if !validDimensions(dimensions) {
		return ErrInvalidDimensions
	}
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(dimensions.Columns), Y: int16(dimensions.Rows)})
}
func (p *Process) Signal(signal Signal) error {
	if signal != Interrupt && signal != Terminate && signal != Hangup && signal != Kill {
		return ErrInvalidSignal
	}
	p.mu.RLock()
	process := p.process
	p.mu.RUnlock()
	if process == 0 {
		return os.ErrProcessDone
	}
	if signal == Interrupt {
		// ConPTY translates the terminal ETX byte into the same Ctrl+C input
		// event an interactive console receives. Do not terminate the shell:
		// the foreground command must be interrupted while the session remains
		// usable for subsequent commands.
		foreground := p.foregroundTree()
		if _, err := p.WriteContext(context.Background(), []byte{0x03}); err != nil { // under writeMu: never races a write in progress
			return err
		}
		p.terminateIfStillRunning(foreground)
		return nil
	}
	code := uint32(1)
	if signal == Kill {
		code = 137
	}
	if err := windows.TerminateProcess(process, code); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// processKey names one process instance: a pid alone could be reused after the process ends.
type processKey struct {
	pid     uint32
	created int64
}

// foregroundTree is the program cmd.exe is running for the user, with positive evidence only: a direct child of the shell, in the job,
// that the shell holds a process handle to (cmd waits on a foreground program; at an idle prompt and for `start /b` it holds none), the
// newest such child, and everything under it. Windows has no foreground process group, so nothing weaker is used: with any other shell
// (PowerShell keeps handles to job workers and background processes while idle), an unreadable handle table or no held child the result is
// empty and a Ctrl-C terminates nothing (missing a cancel is better than killing background work), like Unix where Ctrl-C at an
// idle prompt never touches background jobs.
func (p *Process) foregroundTree() []processKey {
	if p.job == 0 || !p.shellIsCommandInterpreter() {
		return nil
	}
	held := processesHeldBy(p.processID) // positive evidence only: the shell waits on this child; unreadable table: no foreground
	if held == nil {
		return nil
	}
	members := map[uint32]bool{}
	for _, pid := range jobProcessIDs(p.job) {
		members[pid] = true
	}
	parents := processParents()
	var newest processKey
	for pid := range members {
		if parents[pid] != p.processID || pid == p.processID || !held[pid] {
			continue
		}
		if key, ok := processInstance(pid); ok && key.created > newest.created {
			newest = key
		}
	}
	if newest.pid == 0 {
		return nil
	}
	tree := []processKey{newest}
	in := map[uint32]bool{newest.pid: true}
	for grew := true; grew; {
		grew = false
		for pid := range members {
			if !in[pid] && in[parents[pid]] {
				if key, ok := processInstance(pid); ok {
					tree, in[pid], grew = append(tree, key), true, true
				}
			}
		}
	}
	return tree
}

// terminateIfStillRunning is the fallback after an ETX: a pipe ETX does not signal every console program (measured on Windows 11:
// cmd.exe running ping -t ignores it, with or without queued input). It waits up to 100 ms for native handling, returning at once when the
// foreground tree has ended, then terminates what of it is still the same running process. The shell survives and the job bounds everything.
func (p *Process) terminateIfStillRunning(tree []processKey) {
	if len(tree) == 0 {
		return
	}
	for waited := 0; waited < 10; waited++ {
		if !anyRunning(tree) {
			return
		}
		//paperboat:allow-source-policy sleep owner=windows-conpty reason=bounded-native-ctrl-c-grace-before-descendant-fallback
		time.Sleep(10 * time.Millisecond)
	}
	for _, key := range tree {
		handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, key.pid)
		if err != nil {
			continue
		}
		if current, ok := processTimes(handle); ok && current == key.created {
			_ = windows.TerminateProcess(handle, 130)
		}
		windows.CloseHandle(handle)
	}
}

func anyRunning(tree []processKey) bool {
	for _, key := range tree {
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, key.pid)
		if err != nil {
			continue
		}
		var code uint32
		running := windows.GetExitCodeProcess(handle, &code) == nil && code == 259 // STILL_ACTIVE
		current, ok := processTimes(handle)
		windows.CloseHandle(handle)
		if running && ok && current == key.created {
			return true
		}
	}
	return false
}

func processInstance(pid uint32) (processKey, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return processKey{}, false
	}
	defer windows.CloseHandle(handle)
	created, ok := processTimes(handle)
	return processKey{pid: pid, created: created}, ok
}

func processTimes(handle windows.Handle) (int64, bool) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return created.Nanoseconds(), true
}

// processParents maps every process id to its parent's id.
func processParents() map[uint32]uint32 {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)
	parents := map[uint32]uint32{}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parents[entry.ProcessID] = entry.ParentProcessID
	}
	return parents
}

func jobProcessIDs(job windows.Handle) []uint32 {
	if job == 0 { // no job (a Process over a plain pipe): QueryInformationJobObject(NULL) would list unrelated processes
		return nil
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST has two DWORD counters followed by a
	// pointer-width process ID array. Cap collection to 256 processes so signal
	// handling remains bounded even under a hostile workload.
	const maximumProcesses = 256
	buffer := make([]byte, 8+maximumProcesses*int(unsafe.Sizeof(uintptr(0))))
	var returned uint32
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&buffer[0])), uint32(len(buffer)), &returned); err != nil {
		return nil
	}
	assigned := *(*uint32)(unsafe.Pointer(&buffer[4]))
	if assigned > maximumProcesses {
		assigned = maximumProcesses
	}
	result := make([]uint32, 0, assigned)
	offset := 8
	stride := int(unsafe.Sizeof(uintptr(0)))
	for index := uint32(0); index < assigned && offset+stride <= len(buffer); index++ {
		pid := *(*uintptr)(unsafe.Pointer(&buffer[offset]))
		if pid != 0 && pid <= uintptr(^uint32(0)) {
			result = append(result, uint32(pid))
		}
		offset += stride
	}
	return result
}
func (p *Process) Wait(ctx context.Context) (ExitResult, error) {
	select {
	case <-p.done:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.result, p.waitErr
	case <-ctx.Done():
		return ExitResult{}, ctx.Err()
	}
}
func (p *Process) CloseIO() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.inputClosed = true
		p.mu.Unlock()
		p.closeErr = errors.Join(p.input.Close(), p.output.Close())
		p.closeConsole()
		var processErr, jobErr error
		if p.process != 0 {
			processErr = windows.Close(p.process)
		}
		if p.job != 0 {
			jobErr = windows.Close(p.job)
		}
		p.closeErr = errors.Join(p.closeErr, processErr, jobErr)
	})
	return p.closeErr
}
func (p *Process) closeConsole() {
	p.consoleOnce.Do(func() { windows.ClosePseudoConsole(p.console) })
}
func (p *Process) Terminate(ctx context.Context, grace time.Duration) (ExitResult, error) {
	if grace < 0 {
		return ExitResult{}, ErrInvalidCommand
	}
	select {
	case <-p.done:
		result, err := p.Wait(context.Background())
		if closeErr := p.CloseIO(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return result, err
	default:
	}
	if err := p.Signal(Terminate); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return ExitResult{}, err
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	result, err := p.Wait(graceCtx)
	cancel()
	if err == nil {
		return result, p.CloseIO()
	}
	if ctx.Err() != nil {
		return ExitResult{}, ctx.Err()
	}
	if err := p.Signal(Kill); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return ExitResult{}, err
	}
	result, err = p.Wait(ctx)
	if closeErr := p.CloseIO(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return result, err
}
func (p *Process) wait() {
	_, waitErr := windows.WaitForSingleObject(p.process, windows.INFINITE)
	var code uint32
	if waitErr == nil {
		waitErr = windows.GetExitCodeProcess(p.process, &code)
	}
	result := ExitResult{Code: int(code), ExitedAt: time.Now().UTC()}
	p.mu.Lock()
	p.result, p.waitErr = result, waitErr
	p.mu.Unlock()
	close(p.done)
	// ConPTY does not close its output pipe merely because the attached process
	// exited. Closing the pseudoconsole releases the final VT output and lets the
	// session capture loop observe EOF, after which it can publish exact exit
	// state. Keep this on the waiter goroutine while capture drains output: the
	// Windows API may wait for pending pseudoconsole output to be consumed.
	p.closeConsole()
}

// anonymousPipe creates the pipe pair used by ConPTY. The pseudoconsole API
// expects the console-side endpoint to remain inheritable while the host-side
// endpoint stays private. Clearing inheritance on both ends makes the
// subsequent ConPTY child launch fail from a service process.
func anonymousPipe(inheritRead bool) (windows.Handle, windows.Handle, error) {
	var read, write windows.Handle
	// CreatePseudoConsole requires the console-side endpoint to be inheritable.
	// CreatePipe does not make handles inheritable when its security attributes
	// argument is nil, even though the subsequent child launch uses the
	// pseudoconsole attribute. Keep the host-side endpoint private immediately
	// after creating the pair.
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	if err := windows.CreatePipe(&read, &write, &security, 0); err != nil {
		return 0, 0, err
	}
	private := read
	if inheritRead {
		private = write
	}
	if err := windows.SetHandleInformation(private, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.Close(read)
		windows.Close(write)
		return 0, 0, err
	}
	return read, write, nil
}
func closeHandles(values ...windows.Handle) {
	for _, value := range values {
		if value != 0 {
			windows.Close(value)
		}
	}
}
func commandLine(path string, args []string) ([]uint16, error) {
	line := syscall.EscapeArg(path)
	for _, argument := range args {
		if strings.ContainsRune(argument, '\x00') {
			return nil, ErrInvalidCommand
		}
		line += " " + syscall.EscapeArg(argument)
	}
	return windows.UTF16FromString(line)
}
func environmentBlock(environment []string) (*uint16, error) {
	if len(environment) == 0 {
		return nil, nil
	}
	copyEnvironment := append([]string(nil), environment...)
	// CreateProcess requires a sorted, double-NUL-terminated UTF-16 block.
	sortStrings(copyEnvironment)
	// UTF16PtrFromString rejects embedded NULs, but the NUL separators are
	// required by the CreateProcess environment-block contract. Encode the
	// complete block directly and retain both terminators after the last entry.
	block := utf16.Encode([]rune(strings.Join(copyEnvironment, "\x00") + "\x00\x00"))
	return &block[0], nil
}
func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && strings.ToUpper(values[cursor]) < strings.ToUpper(values[cursor-1]); cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}
func validDimensions(dimensions Dimensions) bool {
	return dimensions.Columns >= 1 && dimensions.Columns <= 1000 && dimensions.Rows >= 1 && dimensions.Rows <= 1000
}
func validEnvironment(environment []string) bool {
	if len(environment) > maximumProcessEnvironmentEntries {
		return false
	}
	seen := make(map[string]bool, len(environment))
	total := 0
	for _, entry := range environment {
		total += len(entry)
		key, value, ok := strings.Cut(entry, "=")
		folded := strings.ToUpper(key)
		if !ok || key == "" || len(entry) > maximumProcessEnvironmentEntryBytes || total > maximumProcessEnvironmentBytes || strings.ContainsAny(key, "\x00\r\n") || strings.ContainsRune(value, '\x00') || seen[folded] {
			return false
		}
		seen[folded] = true
	}
	return true
}
func validArguments(arguments []string) bool {
	if len(arguments) > 64 {
		return false
	}
	total := 0
	for _, argument := range arguments {
		total += len(argument)
		if len(argument) > 4096 || total > 64<<10 || strings.ContainsRune(argument, '\x00') {
			return false
		}
	}
	return true
}
func ValidateProcessPolicy(path string, arguments, environment []string) (string, error) {
	resolved, err := validateExecutable(path)
	if err != nil || !validArguments(arguments) || !validEnvironment(environment) {
		return "", ErrInvalidCommand
	}
	return resolved, nil
}
func validateExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrInvalidCommand
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrInvalidCommand
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrInvalidCommand
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(resolved))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", ErrInvalidCommand
	}
	return resolved, nil
}
func resolveDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrInvalidCWD
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", ErrInvalidCWD
	}
	return filepath.Clean(resolved), nil
}
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
