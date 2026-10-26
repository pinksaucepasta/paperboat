package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/mcpserver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

func mcpCommand() *cobra.Command {
	c := &cobra.Command{
		Use: "mcp", Short: "Serve Paperboat tools over MCP stdio",
		Long: "Serve list_machines, exec and send_file using your existing Paperboat login.\n" +
			"For automation, first sign in with pb login --api-token-file.\n" +
			"Exec uses argv without a shell, bounded output and a maximum five-minute deadline.\n" +
			"send_file requires --allow-send-dir and delivers to the receiving machine's Inbox.\n" +
			"This uses existing CLI access; the local Paperboat daemon and enrolled source are still required.\n" +
			"Standard input/output are reserved for MCP; configure your MCP client to launch pb mcp.",
		Args: commandArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			backend := &mcpBackend{command: c}
			paths, _ := c.Flags().GetStringArray("allow-send-dir")
			if err := backend.openRoots(paths); err != nil {
				return err
			}
			defer backend.closeRoots()
			input, ok := c.InOrStdin().(io.ReadCloser)
			if !ok {
				return errors.New("MCP input must be closeable for cancellation")
			}
			return mcpserver.New(buildinfo.Version, backend).Serve(c.Context(), input, mcpOutputCloser{c.OutOrStdout()})
		},
	}
	c.Flags().StringArray("allow-send-dir", nil, "allow sending files within this local directory (repeatable; symlinks cannot escape)")
	return c
}

type mcpOutputCloser struct{ io.Writer }

func (mcpOutputCloser) Close() error { return nil }

type mcpBackend struct {
	command *cobra.Command
	roots   []mcpSendRoot
}

// Build an isolated Cobra invocation solely to bind the owning Go action's
// existing flags. Do not execute the root or apply interactive preferences.
func (b *mcpBackend) invocation(ctx context.Context, name string, output io.Writer, input string, flags ...string) (*cobra.Command, error) {
	root := newRootCommand()
	root.SetContext(ctx)
	root.SetOut(output)
	root.SetErr(io.Discard)
	root.SetIn(strings.NewReader(input))
	c, _, err := root.Find([]string{name})
	if err != nil {
		return nil, err
	}
	c.SetContext(ctx)
	for _, key := range []string{"config", "server", "workspace"} {
		value, _ := b.command.Flags().GetString(key)
		if value != "" {
			flags = append(flags, "--"+key, value)
		}
	}
	flags = append(flags, "--json", "--no-customization")
	if err := c.ParseFlags(flags); err != nil {
		return nil, err
	}
	return c, nil
}

func (b *mcpBackend) Machines(ctx context.Context, limit int) (mcpserver.MachinePage, error) {
	c, err := b.invocation(ctx, "machine", io.Discard, "")
	if err != nil {
		return mcpserver.MachinePage{}, err
	}
	action := actionContext(c, nil)
	d, err := buildDeps(action)
	if err != nil {
		return mcpserver.MachinePage{}, mcpSafeError{err}
	}
	client, err := newWorkspaceAPIClient(action, d.cfg.ServerURL, d.auth)
	if err != nil {
		return mcpserver.MachinePage{}, mcpSafeError{err}
	}
	machines, total, err := client.ListUserMachinesPage(ctx, nil, limit)
	if err != nil {
		return mcpserver.MachinePage{}, mcpSafeError{err}
	}
	page := mcpserver.MachinePage{Machines: []mcpserver.Machine{}, Total: total, Truncated: total > len(machines)}
	for _, machine := range machines {
		page.Machines = append(page.Machines, mcpserver.Machine{
			ID: machine.ID, Name: machine.Alias, Platform: machine.Platform, Online: machine.Online,
		})
	}
	return page, nil
}

func (b *mcpBackend) Exec(ctx context.Context, args mcpserver.ExecArgs) (mcpserver.ExecResult, error) {
	output := newMCPExecOutput()
	c, err := b.invocation(ctx, "exec", output, args.Stdin)
	if err != nil {
		return mcpserver.ExecResult{}, err
	}
	request := tunnel.ExecRequest{Argv: args.Argv, CWD: args.Directory, Timeout: time.Duration(args.TimeoutSeconds) * time.Second}
	err = actionRemoteExec(actionContext(c, nil), args.Machine, request, true)
	return output.finish(err), nil
}

func (b *mcpBackend) SendFile(ctx context.Context, args mcpserver.SendArgs) (mcpserver.SendResult, error) {
	path, cleanup, err := b.prepareSend(ctx, args.Path)
	if err != nil {
		return mcpserver.SendResult{}, err
	}
	defer cleanup()
	output := &mcpBoundedOutput{}
	c, err := b.invocation(ctx, "send", output, "", "--to", args.Machine, "--session=")
	if err != nil {
		return mcpserver.SendResult{}, err
	}
	if err := runSendCommand(c, []string{path}); err != nil {
		return mcpserver.SendResult{}, mcpSafeError{err}
	}
	var result struct {
		Data json.RawMessage `json:"data"`
	}
	if output.truncated || json.Unmarshal(output.data, &result) != nil || len(result.Data) == 0 {
		return mcpserver.SendResult{}, errors.New("transfer finished but its receipt is unavailable; check the receiving Inbox before retrying")
	}
	return mcpserver.SendResult{Receipt: result.Data}, nil
}

type mcpBoundedOutput struct {
	data      []byte
	truncated bool
}

func (w *mcpBoundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if len(data) > mcpserver.MaxOutputBytes-len(w.data) {
		w.truncated = true
		data = data[:mcpserver.MaxOutputBytes-len(w.data)]
	}
	w.data = append(w.data, data...)
	return n, nil
}

// Preserve typed causes for callers while exposing only the CLI's safe message to MCP.
type mcpSafeError struct{ cause error }

func (e mcpSafeError) Error() string { return userFacingError(e.cause) }
func (e mcpSafeError) Unwrap() error { return e.cause }
