package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// guardPAC hands this user's private-preview PAC to the root machine guard (macOS), which serves it from the one
// system auto-proxy URL. hostd never changes a system proxy setting itself.
type guardPAC struct {
	set   func(ctx context.Context, body string) error
	fetch func(ctx context.Context, url string) (string, error)
}

func (g guardPAC) Recover(context.Context) error { return nil } // the guard drops the PAC when hostd's connection closes

func (g guardPAC) Install(ctx context.Context, pacURL string) error {
	body, err := g.fetch(ctx, pacURL)
	if err != nil {
		return err
	}
	return g.set(ctx, body)
}

func (g guardPAC) Refresh(ctx context.Context, pacURL string) error { return g.Install(ctx, pacURL) }

func (g guardPAC) Remove(ctx context.Context) error { return g.set(ctx, "") }

// fetchLoopbackPAC reads the PAC hostd's own proxy serves; the URL is hostd's loopback listener.
func fetchLoopbackPAC(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("read the private preview PAC: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read the private preview PAC: status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<10+1))
	if err != nil || len(body) > 32<<10 {
		return "", fmt.Errorf("read the private preview PAC: %w (limit 32 KiB)", err)
	}
	return string(body), nil
}
