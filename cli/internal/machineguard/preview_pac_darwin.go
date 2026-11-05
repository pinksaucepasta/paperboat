//go:build darwin

package machineguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/privateproxyconfig"
)

const previewProxyJournal = "system-proxy.json"

func previewProxyManager(stateDir string, runner privateproxyconfig.CommandRunner) (*privateproxyconfig.Manager, error) {
	return privateproxyconfig.New(filepath.Join(stateDir, previewProxyJournal), privateproxyconfig.NewMacOSAdapter(runner))
}

// startPreviewPAC serves the PAC endpoint and points the system auto-proxy at it. A failure is not fatal: machine
// names keep working, private previews stay unrouted. The setting survives a guard restart; purge restores it.
func (g *guardServer) startPreviewPAC(ctx context.Context) func() {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(PreviewPACPort)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "private preview PAC endpoint unavailable: %v\n", err)
		return func() {}
	}
	owner := func(r *http.Request) (uint32, error) {
		conn, _ := r.Context().Value(previewConnKey{}).(net.Conn)
		if conn == nil {
			return 0, errAttributionNotFound
		}
		tuple, err := clientTuple(conn)
		if err != nil {
			return 0, err
		}
		return attributeOwner(tuple, platformLookupOwners)
	}
	server := &http.Server{
		Handler: g.previewPACHandler(owner),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, previewConnKey{}, c)
		},
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       10 * time.Second,
	}
	go server.Serve(listener)
	manager, err := previewProxyManager(g.cfg.StateDir, privateproxyconfig.ExecRunner{})
	if err == nil {
		err = manager.Recover(ctx)
	}
	if err == nil {
		err = manager.Install(ctx, PreviewPACURL())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "private preview system proxy not set: %v\n", err)
	}
	return func() { _ = server.Close() }
}

type previewConnKey struct{}

// removePreviewProxy restores the system auto-proxy setting from before Paperboat set it.
func removePreviewProxy(ctx context.Context, stateDir string) error {
	return removePreviewProxyWith(ctx, stateDir, privateproxyconfig.ExecRunner{})
}

func removePreviewProxyWith(ctx context.Context, stateDir string, runner privateproxyconfig.CommandRunner) error {
	manager, err := previewProxyManager(stateDir, runner)
	if err != nil {
		return err
	}
	if err := manager.Remove(ctx); err != nil {
		return fmt.Errorf("restore the system auto-proxy setting: %w", err)
	}
	return nil
}

// previewProxyLeftover reports a system proxy journal purge did not clear.
func previewProxyLeftover(stateDir string) error {
	if _, err := os.Stat(filepath.Join(stateDir, previewProxyJournal)); err == nil {
		return fmt.Errorf("the system auto-proxy setting was not restored (%s)", previewProxyJournal)
	}
	return nil
}
