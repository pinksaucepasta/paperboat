package tunnelenrollment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

type blockedFailureAssemblySource struct {
	entered chan struct{}
	release chan struct{}
	cause   error
}

func (s *blockedFailureAssemblySource) ResolveProductionAssembly(ctx context.Context, _ ActivationRequest, _ CredentialSigner) (tunnelmanager.ProductionAssemblyConfig, error) {
	close(s.entered)
	select {
	case <-s.release:
		return tunnelmanager.ProductionAssemblyConfig{}, s.cause
	case <-ctx.Done():
		return tunnelmanager.ProductionAssemblyConfig{}, ctx.Err()
	}
}

// Done marks the exact point at which the same-request caller waits on ready.
type activationWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *activationWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestActivationShutdownPublishesSameFinalFailureToWaiter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cause := errors.New("private assembly source failure")
	source := &blockedFailureAssemblySource{entered: make(chan struct{}), release: make(chan struct{}), cause: cause}
	activator, err := NewProductionAssemblyActivator(ProductionAssemblyActivatorConfig{Credentials: &activatorCredentials{}, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if err := activator.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := activator.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	request := activationRequestFixture()
	creator := make(chan error, 1)
	go func() { _, err := activator.Activate(ctx, request); creator <- err }()
	select {
	case <-source.entered:
	case <-ctx.Done():
		t.Fatal("source did not start")
	}
	waitContext := &activationWaitContext{Context: ctx, waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { _, err := activator.Activate(waitContext, request); waiter <- err }()
	select {
	case <-waitContext.waiting:
	case <-ctx.Done():
		t.Fatal("same-request caller did not wait")
	}
	if err := activator.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(source.release)
	var creatorErr, waiterErr error
	select {
	case creatorErr = <-creator:
	case <-ctx.Done():
		t.Fatal("creator did not complete")
	}
	select {
	case waiterErr = <-waiter:
	case <-ctx.Done():
		t.Fatal("waiter did not complete")
	}
	if creatorErr != waiterErr || !errors.Is(creatorErr, cause) || !errors.Is(creatorErr, ErrUnavailable) {
		t.Fatal("creator and waiter did not receive the same final shutdown and source causes")
	}
	activator.mu.Lock()
	retained := len(activator.assemblies)
	activator.mu.Unlock()
	if retained != 0 {
		t.Fatal("closed activator retained an assembly")
	}
}
