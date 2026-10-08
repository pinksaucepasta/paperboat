//go:build windows

package updated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func TestWindowsControlClientCancelsBlockedPipeAndFreshRequestRecovers(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf(`\\.\pipe\pb-observability-updater-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")", InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	requestRead, firstClosed, serverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fixtureCtx, stopFixture := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stopFixture()
		_ = listener.Close()
		select {
		case <-serverDone:
		case <-time.After(4 * time.Second):
			t.Error("fixture pipe worker did not join")
		}
	})
	go func() {
		defer close(serverDone)
		for attempt := 0; attempt < 2; attempt++ {
			connection, err := listener.Accept()
			if err != nil {
				if fixtureCtx.Err() == nil {
					t.Error("fixture pipe accept failed")
				}
				return
			}
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			var request ControlRequest
			if err := json.NewDecoder(connection).Decode(&request); err != nil {
				connection.Close()
				t.Error("fixture request decode failed")
				return
			}
			if attempt == 0 {
				close(requestRead)
				_, _ = io.Copy(io.Discard, connection)
				connection.Close()
				close(firstClosed)
			} else {
				_ = json.NewEncoder(connection).Encode(ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: "2026.10.08.1"})
				connection.Close()
			}
		}
	}()
	client, err := NewClient(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Status(ctx); done <- err }()
	select {
	case <-requestRead:
	case <-time.After(3 * time.Second):
		t.Fatal("updater request did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("pipe cancellation cause lost")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked pipe survived cancellation")
	}
	select {
	case <-firstClosed:
	case <-time.After(time.Second):
		t.Fatal("cancelled pipe did not close")
	}
	response, err := client.Status(context.Background())
	if err != nil || response.Version != "2026.10.08.1" {
		t.Fatal("fresh pipe request did not recover")
	}
	<-serverDone
}
