package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

const maxLogLines = 10000

// splitComponent takes the optional relay|edge word out of args, wherever it stands among the flags.
func splitComponent(args []string) (component string, rest []string, err error) {
	for _, arg := range args {
		if arg != "relay" && arg != "edge" {
			rest = append(rest, arg)
		} else if component != "" {
			return "", nil, commandError("name at most one component: relay or edge")
		} else {
			component = arg
		}
	}
	return component, rest, nil
}

// componentContainers are the containers of the installation in dir that component names (all of them when empty),
// keyed by the name the operator knows them by.
func componentContainers(dir, component string) (map[string]string, error) {
	config, err := selfhost.ReadConfig(dir)
	if err != nil {
		return nil, fmt.Errorf("read the installation (is pbh installed?): %w", err)
	}
	found := map[string]string{}
	for _, c := range config.Components {
		name := c.Capability
		if name == "tunnel" {
			name = "edge"
		}
		if component == "" || component == name {
			found[name] = containers.ContainerName(dir, c.Capability)
		}
	}
	if len(found) == 0 {
		return nil, commandError("this installation does not run the " + component + "; it runs " + installationMode(config))
	}
	return found, nil
}

// logsCommand is `pbh logs`: docker logs of the relay and/or edge container, bounded by --lines and --since.
func logsCommand(ctx context.Context, args []string, out io.Writer) error {
	component, rest, err := splitComponent(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	follow := fs.Bool("follow", false, "keep streaming new lines until interrupted")
	since := fs.Duration("since", 0, "only lines newer than this, for example 10m or 2h")
	lines := fs.Int("lines", 200, "most recent lines to show per component (at most "+strconv.Itoa(maxLogLines)+")")
	dir := fs.String("state-dir", defaultState, "installation state directory")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out, "logs", fs)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return commandError("unexpected logs arguments; use pbh logs [--follow] [relay|edge]")
	}
	if *lines < 1 || *lines > maxLogLines || *since < 0 {
		return commandError("--lines must be 1 to " + strconv.Itoa(maxLogLines) + " and --since must not be negative")
	}
	if os.Geteuid() != 0 && *dir == defaultState {
		return elevated(ctx, append([]string{"logs"}, args...), out)
	}
	targets, err := componentContainers(*dir, component)
	if err != nil {
		return err
	}
	dockerArgs := []string{"logs", "--tail", strconv.Itoa(*lines)}
	if *since > 0 {
		dockerArgs = append(dockerArgs, "--since", since.String())
	}
	if *follow {
		dockerArgs = append(dockerArgs, "--follow")
	}
	var lock sync.Mutex
	var group sync.WaitGroup
	failures := make(chan error, len(targets))
	for name, container := range targets {
		prefix := ""
		if len(targets) > 1 {
			prefix = name + " | "
		}
		cmd := exec.CommandContext(ctx, "docker", append(append([]string{}, dockerArgs...), container)...)
		writer := &prefixWriter{out: out, prefix: prefix, lock: &lock}
		cmd.Stdout, cmd.Stderr = writer, writer // docker logs replays the container's stderr on its own stderr
		group.Add(1)
		go func() {
			defer group.Done()
			if err := cmd.Run(); err != nil && ctx.Err() == nil {
				failures <- fmt.Errorf("read the %s logs: %w", strings.TrimSuffix(prefix, " | "), err)
			}
			writer.flush()
		}()
	}
	group.Wait()
	close(failures)
	return errors.Join(drain(failures)...)
}

func drain(c chan error) (errs []error) {
	for err := range c {
		errs = append(errs, err)
	}
	return errs
}

// prefixWriter writes whole lines, each starting with prefix; one lock keeps lines of two containers apart.
type prefixWriter struct {
	out    io.Writer
	prefix string
	lock   *sync.Mutex
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
}

func (w *prefixWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = nil
	}
}

func (w *prefixWriter) emit(line []byte) {
	w.lock.Lock()
	defer w.lock.Unlock()
	fmt.Fprint(w.out, w.prefix+string(line))
}

// restartCommand is `pbh restart`: the whole service, or one component. A stopped component container is brought
// back by the service itself (it waits for the node to be ready again), so restart waits for that.
func restartCommand(ctx context.Context, args []string, out io.Writer) error {
	component, rest, err := splitComponent(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("state-dir", defaultState, "installation state directory")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out, "restart", fs)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return commandError("unexpected restart arguments; use pbh restart [relay|edge]")
	}
	if os.Geteuid() != 0 && *dir == defaultState {
		return elevated(ctx, append([]string{"restart"}, args...), out)
	}
	if component == "" {
		if output, err := exec.CommandContext(ctx, "systemctl", "restart", "paperboat-selfhost.service").CombinedOutput(); err != nil {
			return fmt.Errorf("restart the service: %w: %s", err, lastLine(string(output)))
		}
		fmt.Fprintln(out, "Restarted paperboat-selfhost.service. Run pbh status to check it.")
		return nil
	}
	targets, err := componentContainers(*dir, component)
	if err != nil {
		return err
	}
	container := targets[component]
	if output, err := exec.CommandContext(ctx, "docker", "stop", container).CombinedOutput(); err != nil {
		return fmt.Errorf("stop the %s: %w: %s", component, err, lastLine(string(output)))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if output, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", container).Output(); err == nil && strings.TrimSpace(string(output)) == "true" {
			fmt.Fprintf(out, "Restarted the %s.\n", component)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("the %s did not come back within 2 minutes; run pbh logs %s", component, component)
}
