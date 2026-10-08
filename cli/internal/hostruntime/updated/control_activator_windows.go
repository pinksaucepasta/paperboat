//go:build windows

package updated

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// The activator temporarily owns the existing control endpoint. Its journal
// remains the only transaction state, and no scheduler runs in this process.
type windowsActivatorControl struct {
	config     WindowsConfig
	listener   net.Listener
	done       chan struct{}
	stopOnce   sync.Once
	mu         sync.Mutex
	connection net.Conn
	stopping   bool
	err        error
}

func waitWindowsUpdaterStopped(ctx context.Context, ownerSID string) error {
	_, name, err := windowsInstanceServiceNames(ownerSID)
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if err != nil {
		return err
	}
	defer item.Close()
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		status, err := item.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		select {
		case <-bounded.Done():
			return bounded.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func startWindowsActivatorControl(ctx context.Context, config WindowsConfig) (*windowsActivatorControl, error) {
	listener, err := winio.ListenPipe(config.ControlSocket, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GRGW;;;SY)(A;;GRGW;;;" + config.OwnerSID + ")", InputBufferSize: 16 << 10, OutputBufferSize: 16 << 10})
	if err != nil {
		return nil, err
	}
	control := &windowsActivatorControl{config: config, listener: listener, done: make(chan struct{})}
	go control.run(ctx)
	go func() {
		select {
		case <-ctx.Done():
			control.Close()
		case <-control.done:
		}
	}()
	return control, nil
}

func (c *windowsActivatorControl) run(ctx context.Context) {
	defer close(c.done)
	for {
		connection, err := c.listener.Accept()
		if err != nil {
			c.mu.Lock()
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, winio.ErrPipeListenerClosed) && ctx.Err() == nil {
				c.err = err
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		if c.stopping {
			c.mu.Unlock()
			connection.Close()
			return
		}
		c.connection = connection
		c.mu.Unlock()
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		// The shared decoder maintains the same framing and request validation.
		_, _ = handleWindowsControl(requestCtx, connection, c.invoke)
		cancel()
		connection.Close()
		c.mu.Lock()
		c.connection = nil
		c.mu.Unlock()
	}
}

func (c *windowsActivatorControl) Close() error {
	c.stopOnce.Do(func() {
		c.listener.Close()
		c.mu.Lock()
		c.stopping = true
		if c.connection != nil {
			c.connection.Close()
		}
		c.mu.Unlock()
	})
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *windowsActivatorControl) invoke(ctx context.Context, request ControlRequest) (ControlResponse, error) {
	if err := ctx.Err(); err != nil {
		return ControlResponse{}, err
	}
	journal, err := loadWindowsActivationJournalForController(c.config)
	if err != nil {
		return ControlResponse{}, err
	}
	response := ControlResponse{Schema: ControlProtocolV1, Status: "ok", Version: journal.PreviousVersion, Transaction: windowsTransactionState(journal)}
	response.Pending = journal.Stage != windowsActivationAwaitingApproval && journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack
	if journal.Stage == windowsActivationCommitted {
		response.Version = journal.Version
		response.Updated = true
	}
	if journal.Stage == windowsActivationRolledBack {
		response.ActivationFailure = "activation_failed"
	}
	if journal.Stage == windowsActivationRollingBack || journal.Stage == windowsActivationRollbackReady {
		response.ActivationFailure = "recovery_pending"
	}
	if request.Operation == "settings" {
		// Preferences affect future scheduling. They never revoke an approved
		// transaction's recovery ownership, including an opt-out during monitoring.
		if _, err := machineUpdateSettings(c.config.StateRoot, c.config.AutomaticChecks, request.Settings); err != nil {
			return response, err
		}
	}
	if err := populateMachineSettings(&response, c.config.StateRoot, c.config.AutomaticChecks); err != nil {
		return response, err
	}
	if request.Operation == "status" && journal.Stage != windowsActivationCommitted && journal.Stage != windowsActivationRolledBack {
		if err := verifyWindowsPreparedCandidate(ctx, journal); err != nil {
			return response, err
		}
		candidate := journal.Candidate
		response.Candidate = &candidate
	}
	if request.Operation != "status" && request.Operation != "settings" {
		return response, ErrWindowsActivationUnavailable
	}
	return response, nil
}

// Terminal retirement retries must reuse the restored native control owner.
// Maintenance starts that owner before monitoring, so those recovery stages
// likewise leave its endpoint intact.
func windowsActivatorNeedsControl(journal windowsActivationJournal) bool {
	switch journal.Stage {
	case windowsActivationCommitted, windowsActivationRolledBack:
		return false
	case windowsActivationServicesLive, windowsActivationCommitReady, windowsActivationRollbackReady:
		return !journal.Release.SupervisorMaintenance
	default:
		return true
	}
}
