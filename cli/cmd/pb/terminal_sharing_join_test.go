package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type joinTestConn struct{ tunnel.Conn }

// Lane N, F3-06: the join printed "Shared terminal … view only" before connecting, so a join stuck in
// the native dial looked attached for 90 s. The banner may only follow a connected dial.
func TestSharedJoinAnnouncesTheTerminalOnlyAfterItConnects(t *testing.T) {
	info := resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{}}
	info.Terminal.Auth.Scopes = []string{"terminal:view"}
	var out bytes.Buffer
	failure := errors.New("native dial failed")
	if _, err := dialSharedTerminal(context.Background(), &out, func(context.Context, resolver.ConnectInfo) (tunnel.Conn, error) {
		return nil, failure
	}, info, "terminal_1"); !errors.Is(err, failure) {
		t.Fatalf("dial error = %v", err)
	}
	if strings.Contains(out.String(), "Shared terminal terminal_1") || !strings.Contains(out.String(), "Connecting to shared terminal terminal_1") {
		t.Fatalf("failed join output = %q", out.String())
	}
	out.Reset()
	conn, err := dialSharedTerminal(context.Background(), &out, func(context.Context, resolver.ConnectInfo) (tunnel.Conn, error) {
		return joinTestConn{}, nil
	}, info, "terminal_1")
	if err != nil || conn == nil || !strings.Contains(out.String(), "Shared terminal terminal_1 · view only") {
		t.Fatalf("connected join conn=%v err=%v output=%q", conn, err, out.String())
	}
}
