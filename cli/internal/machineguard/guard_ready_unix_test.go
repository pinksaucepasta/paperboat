//go:build linux || darwin

package machineguard

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitGuardReadyRejectsGuardThatAnswersThenDrops(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "guard.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err == nil {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			if _, err = readFrame(conn); err == nil {
				// Close the listening socket before replying so a second dial cannot succeed.
				listener.Close()
				payload, _ := json.Marshal(response{Status: &RuntimeStatus{Ready: true}})
				if _, _, err = conn.WriteMsgUnix([]byte{0}, nil, nil); err == nil {
					err = writeFrame(conn, payload)
				}
			}
		}
		done <- err
	}()

	readyReplies := 0
	err = waitGuardReady(t.Context(), time.Second, func(ctx context.Context) error {
		// Use the real RPC client. This unprivileged fixture bypasses only Connect's root-owner check.
		raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return err
		}
		client := &Client{conn: raw.(*net.UnixConn)}
		defer client.Close()
		_, err = client.Status(ctx)
		if err == nil {
			readyReplies++
		}
		return err
	})
	listener.Close()
	if replyErr := <-done; replyErr != nil {
		t.Fatal(replyErr)
	}
	if err == nil || readyReplies != 1 {
		t.Fatalf("install readiness after one ready reply and a dropped guard: err=%v, ready replies=%d", err, readyReplies)
	}
}
