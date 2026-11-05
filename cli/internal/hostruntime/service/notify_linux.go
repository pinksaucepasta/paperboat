package service

import (
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
)

type ProcessNotifier struct {
	watchdog time.Duration
	socket   string
	now      func() time.Time
	mu       sync.Mutex
	readyAt  time.Time
	reset    bool
}

// stableAfterReady is how long a ready process must keep running before systemd's restart backoff is reset
// (RESTART_RESET=1, systemd >= 258; older ones ignore it). Resetting at READY would keep a process that
// crashes right after start at the shortest restart delay forever.
const stableAfterReady = time.Minute

type serviceManagerEnvironment struct {
	socket   string
	watchdog time.Duration
}

// captureServiceManager takes the main process's notification socket and watchdog out of the
// environment, once per process, so no child process inherits them: a child's notifications are
// rejected by systemd ("reception only permitted for main PID"), logged on every carrier start (Lane W,
// stg-m1).
func captureServiceManager() (serviceManagerEnvironment, error) {
	watchdog, err := daemon.SdWatchdogEnabled(true)
	socket := os.Getenv("NOTIFY_SOCKET")
	_ = os.Unsetenv("NOTIFY_SOCKET")
	return serviceManagerEnvironment{socket: socket, watchdog: watchdog}, err
}

var serviceManager = sync.OnceValues(captureServiceManager)

func NewProcessNotifier() (*ProcessNotifier, error) {
	environment, err := serviceManager()
	if err != nil {
		return nil, err
	}
	return &ProcessNotifier{watchdog: environment.watchdog, socket: environment.socket, now: time.Now}, nil
}

func (n *ProcessNotifier) WatchdogInterval() time.Duration {
	if n == nil || n.watchdog == 0 {
		return 0
	}
	return n.watchdog / 2
}

func (n *ProcessNotifier) Starting() error { return n.status("starting") }
func (n *ProcessNotifier) Ready() error {
	if n != nil {
		n.mu.Lock()
		n.readyAt = n.now()
		n.mu.Unlock()
	}
	return n.notify(daemon.SdNotifyReady + "\nSTATUS=ready")
}
func (n *ProcessNotifier) Degraded(reason string) error {
	if !safeNotificationStatus(reason) {
		return ErrInvalidDefinition
	}
	return n.status("degraded: " + reason)
}
func (n *ProcessNotifier) Draining() error { return n.status("draining") }
func (n *ProcessNotifier) Stopping() error {
	return n.notify(daemon.SdNotifyStopping + "\nSTATUS=stopping")
}

// Watchdog also resets systemd's restart backoff, once, when the process has been ready for stableAfterReady.
func (n *ProcessNotifier) Watchdog() error {
	if n == nil {
		return n.notify(daemon.SdNotifyWatchdog)
	}
	n.mu.Lock()
	reset := !n.reset && !n.readyAt.IsZero() && n.now().Sub(n.readyAt) >= stableAfterReady
	n.mu.Unlock()
	if !reset {
		return n.notify(daemon.SdNotifyWatchdog)
	}
	err := n.notify(daemon.SdNotifyWatchdog + "\nRESTART_RESET=1")
	if err == nil {
		n.mu.Lock()
		n.reset = true
		n.mu.Unlock()
	}
	return err
}

func (n *ProcessNotifier) status(value string) error { return n.notify("STATUS=" + value) }

func (n *ProcessNotifier) notify(state string) error {
	if n == nil {
		return errors.New("nil process notifier")
	}
	if n.socket == "" {
		return nil
	}
	address := &net.UnixAddr{Name: n.socket, Net: "unixgram"}
	if strings.HasPrefix(n.socket, "@") {
		address.Name = "\x00" + n.socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

func safeNotificationStatus(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}
