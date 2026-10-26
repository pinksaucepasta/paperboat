// Package mcpserver exposes the existing CLI operations over MCP, without owning
// authentication, execution recovery or file transfer protocols.
package mcpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	MaxMessageBytes = 128 << 10
	MaxOutputBytes  = 64 << 10
	MaxMachines     = 100
	MaxTimeout      = 5 * time.Minute
	DefaultTimeout  = 30 * time.Second
)

type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Online   bool   `json:"online"`
}

type MachinePage struct {
	Machines  []Machine `json:"machines"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
}

type ExecArgs struct {
	Machine        string   `json:"machine" jsonschema:"Machine name or ID"`
	Argv           []string `json:"argv" jsonschema:"Exact command arguments; no implicit shell"`
	Stdin          string   `json:"stdin,omitempty"`
	Directory      string   `json:"directory,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"Default 30, maximum 300 seconds"`
}

type ExecResult struct {
	OperationID string   `json:"operation_id,omitempty"`
	Outcome     string   `json:"outcome"`
	ExitCode    *int     `json:"exit_code,omitempty"`
	Stdout      string   `json:"stdout"`
	Stderr      string   `json:"stderr"`
	Truncated   bool     `json:"truncated"`
	ErrorCode   string   `json:"error_code,omitempty"`
	Detail      string   `json:"detail,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

type SendArgs struct {
	Machine        string `json:"machine" jsonschema:"Receiving machine name or ID"`
	Path           string `json:"path" jsonschema:"Absolute local file path within an explicitly allowed send directory"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Default 30, maximum 300 seconds"`
}

type SendResult struct {
	Receipt any `json:"receipt"`
}

// Backend is implemented by the CLI adapter using its existing operation owners.
// Errors must be safe for the caller; never return credentials or raw I/O bodies.
type Backend interface {
	Machines(context.Context, int) (MachinePage, error)
	Exec(context.Context, ExecArgs) (ExecResult, error)
	SendFile(context.Context, SendArgs) (SendResult, error)
}

type Server struct {
	protocol *mcp.Server
	backend  Backend
	active   chan struct{}
}

func New(version string, backend Backend) *Server {
	s := &Server{backend: backend, active: make(chan struct{}, 1)}
	s.protocol = mcp.NewServer(&mcp.Implementation{Name: "paperboat", Version: version}, &mcp.ServerOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Instructions: "Use machine IDs when names are ambiguous. Exec uses exact argv. " +
			"Never retry an execution whose outcome is unknown. send_file delivers to the recipient's Inbox.",
	})
	mcp.AddTool(s.protocol, &mcp.Tool{
		Name: "list_machines", Description: "List up to 100 authorized machines and the server's total.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.machines)
	mcp.AddTool(s.protocol, &mcp.Tool{
		Name: "exec",
		Description: "Run exact argv using existing Paperboat access. " +
			"Output is bounded UTF-8 text; invalid bytes become U+FFFD. " +
			"Use file transfer for binary artifacts; inspect outcome before retrying.",
	}, s.exec)
	mcp.AddTool(s.protocol, &mcp.Tool{
		Name: "send_file",
		Description: "Send an allowed local file to a machine's Inbox " +
			"using Automatic mode and existing approval policy.",
	}, s.send)
	return s
}

func (s *Server) Serve(ctx context.Context, reader io.ReadCloser, writer io.WriteCloser) error {
	return s.protocol.Run(ctx, &mcp.IOTransport{Reader: reader, Writer: writer, MaxLineLength: MaxMessageBytes})
}

func (s *Server) begin(ctx context.Context, seconds int) (context.Context, func(), error) {
	if seconds < 0 || seconds > int(MaxTimeout/time.Second) {
		return nil, nil, errors.New("timeout_seconds must be between 1 and 300, or omitted")
	}
	select {
	case s.active <- struct{}{}:
	default:
		return nil, nil, errors.New("another Paperboat tool is running; wait for its result")
	}
	timeout := DefaultTimeout
	if seconds > 0 {
		timeout = time.Duration(seconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, func() { cancel(); <-s.active }, nil
}

func (s *Server) machines(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, MachinePage, error) {
	ctx, done, err := s.begin(ctx, 0)
	if err != nil {
		return nil, MachinePage{}, err
	}
	defer done()
	out, err := s.backend.Machines(ctx, MaxMachines)
	return nil, out, err
}

func (s *Server) exec(ctx context.Context, _ *mcp.CallToolRequest, args ExecArgs) (*mcp.CallToolResult, ExecResult, error) {
	if strings.TrimSpace(args.Machine) == "" || len(args.Argv) == 0 || len(args.Argv) > 128 || len(args.Stdin) > MaxOutputBytes {
		return nil, ExecResult{}, errors.New("provide a machine, 1–128 argv entries and at most 64 KiB stdin")
	}
	for _, arg := range args.Argv {
		if strings.ContainsRune(arg, 0) {
			return nil, ExecResult{}, errors.New("argv cannot contain NUL bytes")
		}
	}
	ctx, done, err := s.begin(ctx, args.TimeoutSeconds)
	if err != nil {
		return nil, ExecResult{}, err
	}
	defer done()
	out, err := s.backend.Exec(ctx, args)
	if err != nil {
		return nil, out, err
	}
	failed := out.ErrorCode != "" || out.ExitCode != nil && *out.ExitCode != 0
	return &mcp.CallToolResult{IsError: failed}, out, nil
}

func (s *Server) send(ctx context.Context, _ *mcp.CallToolRequest, args SendArgs) (*mcp.CallToolResult, SendResult, error) {
	if strings.TrimSpace(args.Machine) == "" || args.Path == "" {
		return nil, SendResult{}, errors.New("provide a receiving machine and an allowed local file path")
	}
	ctx, done, err := s.begin(ctx, args.TimeoutSeconds)
	if err != nil {
		return nil, SendResult{}, err
	}
	defer done()
	out, err := s.backend.SendFile(ctx, args)
	return nil, out, err
}
