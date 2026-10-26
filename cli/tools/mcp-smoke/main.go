// Command mcp-smoke qualifies the real pb mcp over stdio between fresh containers.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/mcpserver"
)

type suite struct {
	ctx                context.Context
	source, target     string
	sourceID, targetID string
	inbox, run         string
	failed             bool
}

func require(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

func (s *suite) docker(ctx context.Context, container string, args ...string) *exec.Cmd {
	command := []string{"-n", "docker", "exec", "-i", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", container}
	return exec.CommandContext(ctx, "sudo", append(command, args...)...)
}

func (s *suite) python(container, code string) string {
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	command := s.docker(ctx, container, "python3", "-c", code)
	output, err := command.Output()
	require(err == nil, "container observation failed")
	return strings.TrimSpace(string(output))
}

func (s *suite) open() *mcp.ClientSession {
	command := s.docker(s.ctx, s.source, "/home/pbt/.local/bin/pb", "--workspace", "personal", "mcp", "--allow-send-dir", "/tmp/"+s.run+"/allowed")
	command.Stderr = io.Discard
	client := mcp.NewClient(&mcp.Implementation{Name: "paperboat-device-smoke", Version: "1"}, nil)
	session, err := client.Connect(s.ctx, &mcp.CommandTransport{Command: command}, nil)
	require(err == nil, "MCP initialize failed")
	return session
}

func call[T any](ctx context.Context, session *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, T) {
	var out T
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require(err == nil, "MCP "+name+" transport failed")
	data, err := json.Marshal(result.StructuredContent)
	require(err == nil && json.Unmarshal(data, &out) == nil, "MCP structured result invalid")
	return result, out
}

func (s *suite) phase(row, check string, action func() map[string]any) {
	result := map[string]any{"row": row, "check": check, "status": "PASS"}
	defer func() {
		if failure := recover(); failure != nil {
			s.failed = true
			result["status"], result["failure"] = "FAIL", fmt.Sprint(failure)
		}
		if json.NewEncoder(os.Stdout).Encode(result) != nil {
			s.failed = true
		}
	}()
	result["evidence"] = action()
}

func (s *suite) inventory() map[string]any {
	session := s.open()
	defer session.Close()
	result, page := call[mcpserver.MachinePage](s.ctx, session, "list_machines", struct{}{})
	require(!result.IsError && len(page.Machines) >= 2 && len(page.Machines) <= mcpserver.MaxMachines, "MCP inventory failed")
	require(page.Total >= len(page.Machines) && page.Truncated == (page.Total > len(page.Machines)), "MCP total/truncation invalid")
	if !page.Truncated {
		found := map[string]bool{}
		for _, machine := range page.Machines {
			found[machine.ID] = true
		}
		require(found[s.sourceID] && found[s.targetID], "fresh machines absent from inventory")
	}
	return map[string]any{"returned": len(page.Machines), "total": page.Total, "truncated": page.Truncated}
}

func (s *suite) execution() map[string]any {
	session := s.open()
	defer session.Close()
	literal, input := "space ; $(false) ' \\\n", "first\nsecond\x00tail\n"
	code := `import json,sys; print(json.dumps({"args":sys.argv[1:],"stdin":sys.stdin.read()})); print("mcp-stderr",file=sys.stderr)`
	result, out := call[mcpserver.ExecResult](s.ctx, session, "exec", mcpserver.ExecArgs{Machine: s.targetID, Argv: []string{"python3", "-c", code, literal, ""}, Stdin: input})
	require(!result.IsError && out.ExitCode != nil && *out.ExitCode == 0 && out.Outcome == "exited", "MCP exec failed: "+out.ErrorCode)
	var observed struct {
		Args  []string
		Stdin string
	}
	require(json.Unmarshal([]byte(out.Stdout), &observed) == nil && len(observed.Args) == 2 && observed.Args[0] == literal && observed.Args[1] == "" && observed.Stdin == input, "MCP argv/stdin changed")
	require(out.Stderr == "mcp-stderr\n" && out.OperationID != "", "MCP streams/operation ID lost")
	code = fmt.Sprintf(`import os,sys; os.write(1,b"x"*%d); os.write(2,b"y"*%d); sys.exit(7)`, mcpserver.MaxOutputBytes+1, mcpserver.MaxOutputBytes+1)
	result, out = call[mcpserver.ExecResult](s.ctx, session, "exec", mcpserver.ExecArgs{Machine: s.targetID, Argv: []string{"python3", "-c", code}})
	require(result.IsError && out.ExitCode != nil && *out.ExitCode == 7 && out.Outcome == "exited" && out.ErrorCode == "", "truncation lost remote exit result")
	require(out.Truncated && len(out.Stdout)+len(out.Stderr) == mcpserver.MaxOutputBytes && out.OperationID != "", "MCP output bound differs")
	result, _ = call[mcpserver.ExecResult](s.ctx, session, "exec", mcpserver.ExecArgs{Machine: s.targetID, Argv: []string{"true"}, Stdin: strings.Repeat("z", mcpserver.MaxOutputBytes+1)})
	require(result.IsError, "oversized stdin accepted")
	ctx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec", Arguments: s.sleeper("deadline", 10)})
		if err == nil && !r.IsError {
			err = errors.New("deadline reported success")
		}
		done <- err
	}()
	s.await("deadline", "running", 8*time.Second)
	select {
	case err := <-done:
		require(err == nil, "MCP deadline failed")
	case <-ctx.Done():
		panic("MCP deadline did not finish")
	}
	s.await("deadline", "stopped", 20*time.Second)
	return map[string]any{"argv_stdin_exact": true, "streams_separate": true, "output_bytes": mcpserver.MaxOutputBytes, "exit_after_truncation": 7, "deadline_stopped_remote": true}
}

func (s *suite) sleeper(kind string, timeout int) mcpserver.ExecArgs {
	code := "import os,pathlib,time; pathlib.Path(" + strconv.Quote("/tmp/"+s.run+"-"+kind+".pid") + ").write_text(str(os.getpid())); time.sleep(120)"
	return mcpserver.ExecArgs{Machine: s.targetID, Argv: []string{"python3", "-c", code, s.run}, TimeoutSeconds: timeout}
}

func (s *suite) await(kind, wanted string, timeout time.Duration) {
	code := "from pathlib import Path\np=Path(" + strconv.Quote("/tmp/"+s.run+"-"+kind+".pid") + ")\n"
	code += "if not p.exists(): print('waiting')\nelse:\n try: raw=(Path('/proc')/p.read_text()/'cmdline').read_bytes()\n except FileNotFoundError: raw=b''\n print('running' if " + strconv.Quote(s.run) + ".encode() in raw else 'stopped')\n"
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if s.python(s.target, code) == wanted {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	panic("remote " + kind + " process did not become " + wanted)
}

func (s *suite) protocol() map[string]any {
	session := s.open()
	defer session.Close()
	tools, err := session.ListTools(s.ctx, nil)
	require(err == nil && len(tools.Tools) == 3, "MCP tool inventory differs")
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec", Arguments: s.sleeper("cancel", 60)})
		done <- err
	}()
	s.await("cancel", "running", 25*time.Second)
	busy, err := session.CallTool(s.ctx, &mcp.CallToolParams{Name: "list_machines", Arguments: struct{}{}})
	require(err == nil && busy.IsError, "MCP concurrent tool not bounded")
	cancel()
	s.await("cancel", "stopped", 25*time.Second)
	select {
	case err := <-done:
		require(errors.Is(err, context.Canceled), "MCP cancellation outcome differs")
	case <-time.After(5 * time.Second):
		panic("MCP cancellation did not finish")
	}
	session.Close()
	session = s.open()
	go func() {
		_, _ = session.CallTool(s.ctx, &mcp.CallToolParams{Name: "exec", Arguments: s.sleeper("eof", 60)})
	}()
	s.await("eof", "running", 25*time.Second)
	require(session.Close() == nil, "MCP did not close cleanly on EOF")
	s.await("eof", "stopped", 25*time.Second)
	session = s.open()
	defer session.Close()
	frameCtx, stop := context.WithTimeout(s.ctx, 10*time.Second)
	defer stop()
	_, err = session.CallTool(frameCtx, &mcp.CallToolParams{Name: "exec", Arguments: mcpserver.ExecArgs{Machine: s.targetID, Argv: []string{"true"}, Stdin: strings.Repeat("x", mcpserver.MaxMessageBytes)}})
	require(err != nil && frameCtx.Err() == nil, "MCP oversized frame did not close the connection")
	require(session.Ping(frameCtx, nil) != nil && frameCtx.Err() == nil, "MCP oversized frame left session alive")
	return map[string]any{"protocol_only_stdout": true, "concurrency_bounded": true, "cancel_stopped_remote": true, "eof_stopped_remote": true, "frame_bound": true}
}

func (s *suite) prepare() {
	root, name := "/tmp/"+s.run, s.run+".bin"
	code := "from pathlib import Path\nimport os\nos.umask(0o077)\np=Path(" + strconv.Quote(root) + ")\n(p/'allowed').mkdir(parents=True)\n(p/'outside').mkdir()\n"
	code += "(p/'allowed'/" + strconv.Quote(name) + ").write_bytes(b'mcp-fixture')\n(p/'outside'/'outside.bin').write_bytes(b'outside-root')\n(p/'allowed'/'escape.bin').symlink_to('../outside/outside.bin')"
	s.python(s.source, code)
}

func (s *suite) send() map[string]any {
	root, name := "/tmp/"+s.run, s.run+".bin"
	session := s.open()
	defer session.Close()
	result, sent := call[mcpserver.SendResult](s.ctx, session, "send_file", mcpserver.SendArgs{Machine: s.targetID, Path: root + "/allowed/" + name, TimeoutSeconds: 90})
	require(!result.IsError, "MCP send_file failed")
	data, err := json.Marshal(sent.Receipt)
	var batch filetransfer.Batch
	require(err == nil && json.Unmarshal(data, &batch) == nil && len(batch.Transfers) == 1 && batch.BatchID != "", "MCP transfer receipt missing")
	digest := sha256.Sum256([]byte("mcp-fixture"))
	hash := hex.EncodeToString(digest[:])
	item := batch.Transfers[0]
	require(item.SHA256 == hash && item.SourceMachineID == s.sourceID && item.DestinationMachineID == s.targetID, "MCP receipt resource/hash differs")
	s.python(s.target, "from pathlib import Path; import hashlib; p=Path("+strconv.Quote(s.inbox)+")/"+strconv.Quote(name)+"; assert hashlib.sha256(p.read_bytes()).hexdigest()=="+strconv.Quote(hash))
	denied, err := session.CallTool(s.ctx, &mcp.CallToolParams{Name: "send_file", Arguments: mcpserver.SendArgs{Machine: s.targetID, Path: root + "/allowed/escape.bin"}})
	require(err == nil && denied.IsError, "MCP symlink escape accepted")
	s.python(s.target, "from pathlib import Path; assert not (Path("+strconv.Quote(s.inbox)+")/'escape.bin').exists()")
	return map[string]any{"receipt_verified": true, "received_sha256": hash, "symlink_escape_refused": true}
}

func main() {
	s := &suite{}
	flag.StringVar(&s.source, "source-container", "", "fresh source container")
	flag.StringVar(&s.target, "target-container", "", "fresh target container")
	flag.StringVar(&s.sourceID, "source-id", "", "source machine ID")
	flag.StringVar(&s.targetID, "target-id", "", "target machine ID")
	flag.StringVar(&s.inbox, "target-inbox", "", "target's configured Inbox")
	flag.StringVar(&s.run, "run-id", "", "fixture prefix")
	flag.Parse()
	if !regexp.MustCompile(`^lanesdk[0-9a-f]{16}$`).MatchString(s.run) || s.source != s.run+"-c" || s.target != s.run+"-t" || s.sourceID == "" || s.targetID == "" || s.inbox == "" {
		fmt.Fprintln(os.Stderr, "provide both fresh fixture containers, machine IDs, Inbox and run ID")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s.ctx = ctx
	s.prepare()
	s.phase("F24-01", "mcp-inventory", s.inventory)
	s.phase("F24-02", "mcp-exec", s.execution)
	s.phase("F24-03", "mcp-send", s.send)
	s.phase("F24-00", "mcp-stdio", s.protocol)
	if s.failed {
		os.Exit(1)
	}
}
