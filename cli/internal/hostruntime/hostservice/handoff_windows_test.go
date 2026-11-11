//go:build windows

package hostservice

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type orderedApplier struct {
	name   string
	mu     *sync.Mutex
	events *[]string
}

func (a orderedApplier) Apply(context.Context, string) error { a.record("apply"); return nil }
func (a orderedApplier) Close(context.Context) error         { a.record("release"); return nil }
func (a orderedApplier) record(event string) {
	a.mu.Lock()
	*a.events = append(*a.events, a.name+" "+event)
	a.mu.Unlock()
}

// During a hostd update the new generation's host service starts while the old one still holds the account's pipe
// (first-instance pipes refuse a second creator): it waits for the name instead of failing, and the old one
// releases the power policy before its pipe closes, so the new one applies only after that release.
func TestWindowsHostServiceTakesThePipeOverInOrder(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	pipe := `\\.\pipe\PaperboatHostServiceHandoffTest-` + strconv.FormatInt(time.Now().UnixNano(), 10)
	var mu sync.Mutex
	var events []string
	start := func(name string) (context.CancelFunc, chan struct{}, chan error) {
		ready := make(chan struct{})
		server, err := New(Config{
			SocketPath: pipe, StatePath: filepath.Join(t.TempDir(), "availability.json"), SID: user.User.Sid.String(),
			Applier: orderedApplier{name: name, mu: &mu, events: &events}, Version: "test",
			Ready: func() error { close(ready); return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- server.Run(ctx) }()
		return cancel, ready, done
	}
	stopOld, oldReady, oldDone := start("old")
	<-oldReady
	stopNew, newReady, newDone := start("new")
	defer func() { stopNew(); <-newDone }()
	select {
	case <-newReady:
		t.Fatal("the new host service served while the old one held the pipe")
	case <-time.After(300 * time.Millisecond):
	}
	stopOld()
	<-oldDone
	select {
	case <-newReady:
	case <-time.After(5 * time.Second):
		t.Fatal("the new host service did not take the pipe once the old one released it")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events, ", "); got != "old apply, old release, new apply" {
		t.Fatalf("events: %s; want the old release before the new apply", got)
	}
}
