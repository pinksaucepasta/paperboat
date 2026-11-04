// Package paperboat embeds an isolated Paperboat client without a local daemon or pb login state.
package paperboat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/client/api"
	"github.com/pinksaucepasta/paperboat/shared/client/session"
	"github.com/pinksaucepasta/paperboat/shared/client/state"
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
)

const closeTimeout = 5 * time.Second
const maxTokenBytes = 64 << 10

var ErrClosed = errors.New("Paperboat client is closed")

type Options struct {
	ServerURL string
	// Workspace selects an authorized personal or team workspace; empty uses the server default.
	Workspace string
	// Set Token or TokenFile. The supplied API token is exchanged once and never stored.
	Token     string
	TokenFile string
	// StateDir is optional; the default keeps all session state in memory.
	StateDir string
}

type Client struct {
	mu        sync.Mutex
	closed    bool
	lifetime  context.Context
	cancel    context.CancelFunc
	active    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	api       *api.Client
	source    *session.Source
	transport *httptransport.Transport
	directory *state.Directory
}

// New uses ctx for initialization. Close owns the returned client's independent lifetime.
func New(ctx context.Context, options Options) (client *Client, resultErr error) {
	parsed, err := url.Parse(options.ServerURL)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("set ServerURL to an absolute HTTP or HTTPS server URL without credentials, query or fragment")
	}
	if options.Workspace != "" {
		if err := api.ValidateWorkspaceSelector(options.Workspace); err != nil {
			return nil, err
		}
	}
	issuer, err := state.NormalizeIssuer(options.ServerURL)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &Client{lifetime: lifetime, cancel: cancel, transport: httptransport.Default()}
	defer func() {
		if resultErr != nil {
			cancel()
			c.transport.CloseIdleConnections()
			if c.directory != nil {
				resultErr = errors.Join(resultErr, c.directory.Close())
			}
		}
	}()
	var store state.Storage = &state.Memory{}
	if options.StateDir != "" {
		c.directory, err = state.OpenDirectory(options.StateDir)
		if err != nil {
			return nil, fmt.Errorf("open SDK state directory: %w", err)
		}
		store = c.directory
	}
	c.api = &api.Client{BaseURL: issuer, Workspace: options.Workspace, Options: api.CallOptions{
		HTTPClient: &http.Client{Transport: c.transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Headers:    http.Header{"User-Agent": {"paperboat-go/1"}, "X-Paperboat-Client": {"paperboat-go"}, "X-Paperboat-Protocol": {"1"}},
	}}
	c.source = &session.Source{API: c.api, Store: store, Now: time.Now}
	c.api.Credential = c.source.Credential
	current, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("read SDK session: %w", err)
	}
	if current.ID != "" && current.Issuer != issuer {
		return nil, errors.New("SDK state directory belongs to another server; choose another StateDir")
	}
	if current.Revoking {
		if err = c.source.Logout(ctx); err != nil {
			return nil, fmt.Errorf("finish SDK logout: %w", err)
		}
		current = state.Session{}
	}
	if current.ID == "" {
		token, err := readToken(options)
		if err != nil {
			return nil, err
		}
		if err = c.source.Login(ctx, token, "Paperboat Go SDK", runtime.GOOS); err != nil {
			return nil, fmt.Errorf("start SDK session: %w", err)
		}
	} else if _, err = c.source.Credential(ctx); err != nil {
		return nil, fmt.Errorf("resume SDK session: %w", err)
	}
	return c, nil
}

func readToken(options Options) (string, error) {
	if options.Token != "" && options.TokenFile != "" {
		return "", errors.New("set either Token or TokenFile, not both")
	}
	token := options.Token
	if options.TokenFile != "" {
		info, err := os.Stat(options.TokenFile)
		if err != nil {
			return "", fmt.Errorf("inspect API token file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("API token file must be a regular file")
		}
		file, err := os.Open(options.TokenFile)
		if err != nil {
			return "", fmt.Errorf("open API token file: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
		closeErr := file.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return "", fmt.Errorf("read API token file: %w", err)
		}
		token = string(raw)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenBytes || strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("provide a nonempty API token with Token or TokenFile")
	}
	return token, nil
}

func (c *Client) begin(ctx context.Context) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.lifetime, cancel)
	c.active.Add(1)
	return operation, func() { stop(); cancel(); c.active.Done() }, nil
}

// Logout revokes this instance's session and erases it after the server confirms revocation.
// A failed revoke remains durable and can be retried; it cannot authorize new work.
func (c *Client) Logout(ctx context.Context) error {
	ctx, done, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	return c.source.Logout(ctx)
}

// Close cancels and joins active work. Memory sessions are revoked with a bounded request;
// explicit directory sessions remain available for reopen. Repeated calls return the same result.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.cancel()
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		finished := make(chan error, 1)
		go func() {
			c.active.Wait()
			var err error
			if c.directory == nil {
				err = c.source.Logout(ctx)
			}
			c.transport.CloseIdleConnections()
			if c.directory != nil {
				err = errors.Join(err, c.directory.Close())
			}
			finished <- err
		}()
		select {
		case c.closeErr = <-finished:
		case <-ctx.Done():
			c.closeErr = fmt.Errorf("close SDK client: %w", ctx.Err())
		}

	})
	return c.closeErr
}
