//go:build darwin || linux

package browserbroadcastserver

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type delayedPublisherDetach struct {
	session.Service
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	detached chan struct{}
}

func (m *delayedPublisherDetach) Detach(id, attachment string) error {
	first := false
	m.once.Do(func() { first = true; close(m.entered); <-m.release })
	err := m.Service.Detach(id, attachment)
	if first {
		close(m.detached)
	}
	return err
}

func TestPublisherReconnectSurvivesPreviousAttachmentCleanup(t *testing.T) {
	manager, id, generation, original := newPublisherTestSession(t)
	delayed := &delayedPublisherDetach{Service: manager, entered: make(chan struct{}), release: make(chan struct{}), detached: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(delayed.release) }) }
	t.Cleanup(release)
	registry, err := NewRegistry(delayed, original.signingKey)
	if err != nil {
		t.Fatal(err)
	}
	failed := &causePublisher{failure: context.Canceled, closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return failed, nil })
	attachTestViewer(t, manager, id, generation, "browser_first")
	if _, err := registry.Join(context.Background(), id, generation, "browser_first"); err != nil {
		t.Fatal(err)
	}
	writeTestViewer(t, manager, id, generation, "browser_first", 1)
	select {
	case <-delayed.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("previous publisher did not enter cleanup")
	}
	healthy := &signalPublisher{writes: make(chan struct{}, 4), closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return healthy, nil })
	attachTestViewer(t, manager, id, generation, "browser_reconnected")
	if _, err := registry.Join(context.Background(), id, generation, "browser_reconnected"); err != nil {
		t.Fatalf("previous attachment cleanup prevented reconnect: %v", err)
	}
	release()
	select {
	case <-delayed.detached:
	case <-time.After(2 * time.Second):
		t.Fatal("previous publisher cleanup did not finish")
	}
	writeTestViewer(t, manager, id, generation, "browser_reconnected", 1)
	select {
	case <-healthy.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement publisher did not deliver output")
	}
}
