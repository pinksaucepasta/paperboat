package sessionowner

import (
	"context"
	"errors"
	"path/filepath"
	"sort"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/cli/internal/winpipe"
)

// Executions routes explicit executions to session owners. A command runs in
// the owner that was active when it started and stays there across updates
// (owners outlive the hostd process an update replaces), so update maintenance
// never waits for, or cuts, a running command. Calls for an existing command
// go to the generation that holds its operation ID.
func (c *Connections) Executions() (*execprocess.RemoteManager, error) {
	if c == nil {
		return nil, ErrInvalid
	}
	return execprocess.NewRemoteManager(c.execCall)
}

type execRouting struct {
	OperationID string
	Request     *struct {
		OperationID string `json:"operation_id"`
	}
}

func (c *Connections) execCall(ctx context.Context, op string, body []byte) ([]byte, error) {
	var routing execRouting
	if _, err := execprocess.DecodeMessage(body, &routing); err != nil {
		return nil, execprocess.ErrInvalid
	}
	switch op {
	case "begin_update", "end_update":
		// Owner-held commands survive updates; there is nothing to fence.
		return nil, execprocess.ErrUpdateUnavailable
	case "list":
		return c.execList(ctx, body)
	case "start":
		if routing.Request == nil {
			return nil, execprocess.ErrInvalid
		}
		// A retried start of a command an older owner already runs replays there.
		if client := c.execOwner(ctx, routing.Request.OperationID); client != nil {
			return client.Call(ctx, "exec", op, body)
		}
		client, err := c.execClient(ctx, c.active.Generation)
		if err != nil {
			return nil, err
		}
		result, err := client.Call(ctx, "exec", op, body)
		if err == nil {
			c.rememberExec(routing.Request.OperationID, c.active.Generation)
		}
		return result, err
	}
	client := c.execOwner(ctx, routing.OperationID)
	if client == nil {
		return nil, execprocess.ErrNotFound
	}
	return client.Call(ctx, "exec", op, body)
}

// execOwner returns the connected owner holding id, probing live generations
// once on a miss. Archived owners are not started: a running command keeps
// its owner alive, so only finished history can be on a stopped owner.
func (c *Connections) execOwner(ctx context.Context, id string) *workloadbridge.Client {
	if id == "" {
		return nil
	}
	c.mu.Lock()
	generation, known := c.execs[id]
	c.mu.Unlock()
	if known {
		if client, err := c.execClient(ctx, generation); err == nil {
			return client
		}
	}
	probe, _ := execprocess.EncodeMessage(map[string]string{"OperationID": id}, nil)
	for _, generation := range c.liveGenerations(ctx) {
		client, err := c.execClient(c.probeContext(ctx, generation), generation)
		if err != nil {
			continue
		}
		_, err = client.Call(ctx, "exec", "get", probe)
		if err == nil {
			c.rememberExec(id, generation)
			return client
		}
		if errors.Is(err, session.ErrOwnerUnavailable) {
			c.dropExecClient(generation, client)
		}
	}
	return nil
}

func (c *Connections) execList(ctx context.Context, body []byte) ([]byte, error) {
	var all struct{ Snapshots []execprocess.Snapshot }
	for _, generation := range c.liveGenerations(ctx) {
		client, err := c.execClient(c.probeContext(ctx, generation), generation)
		if err != nil {
			continue
		}
		result, err := client.Call(ctx, "exec", "list", body)
		var part struct{ Snapshots []execprocess.Snapshot }
		if err != nil {
			continue
		}
		if _, err := execprocess.DecodeMessage(result, &part); err != nil {
			continue
		}
		all.Snapshots = append(all.Snapshots, part.Snapshots...)
	}
	return execprocess.EncodeMessage(all, nil)
}

// execClient connects to a running owner without starting archived ones.
func (c *Connections) execClient(ctx context.Context, generation string) (*workloadbridge.Client, error) {
	c.mu.Lock()
	client := c.clients[generation]
	c.mu.Unlock()
	if client != nil {
		return client, nil
	}
	d, err := Read(filepath.Join(c.catalog.Root, generation+".json"))
	if err != nil {
		return nil, err
	}
	if d.Generation != generation || !sameAccount(d, c.active) {
		return nil, ErrInvalid
	}
	client, err = Connect(ctx, d)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.clients[generation]; existing != nil {
		client.Close()
		return existing, nil
	}
	c.clients[generation], c.descriptors[generation] = client, d
	return client, nil
}

// dropExecClient forgets a connection whose owner has gone away.
func (c *Connections) dropExecClient(generation string, client *workloadbridge.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients[generation] == client {
		delete(c.clients, generation)
		delete(c.descriptors, generation)
		client.Close()
	}
}

// probeContext dials an older generation's owner without waiting for its pipe: the catalogue keeps every
// generation an update left behind, and waiting winpipe.AbsentWait on each stopped one made every exec start on a
// Windows machine updated ten times take 20 s (victus 2026-11-16).
func (c *Connections) probeContext(ctx context.Context, generation string) context.Context {
	if generation == c.active.Generation {
		return ctx
	}
	return winpipe.AbsentNow(ctx)
}

// liveGenerations lists catalogued generations, active first.
func (c *Connections) liveGenerations(ctx context.Context) []string {
	out := []string{}
	all, _ := c.catalog.ReadAll(ctx, c.active.Instance)
	for _, d := range all {
		if d.Generation != c.active.Generation && sameAccount(d, c.active) {
			out = append(out, d.Generation)
		}
	}
	sort.Strings(out)
	return append([]string{c.active.Generation}, out...)
}

func (c *Connections) rememberExec(id, generation string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.execs == nil || len(c.execs) >= maxRoutedExecs {
		c.execs = make(map[string]string) // shortcut: drops the cache at the bound; misses re-probe live owners.
	}
	c.execs[id] = generation
}

const maxRoutedExecs = 4096
