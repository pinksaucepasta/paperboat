//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

const carrierSchema = "paperboat.native-carrier/v1"

type CarrierConfig struct {
	Schema   string               `json:"schema"`
	Instance string               `json:"instance"`
	Source   installsource.Source `json:"source"`
	// Generation names this carrier's own native unit or service, lock and control
	// socket (the source SHA-256 prefix, like session owners).
	Generation             string            `json:"generation,omitempty"`
	Executable             string            `json:"executable"`
	UID                    int               `json:"uid"`
	GID                    int               `json:"gid"`
	OwnerSID               string            `json:"owner_sid,omitempty"`
	User                   string            `json:"user"`
	Group                  string            `json:"group"`
	RuntimeRoot            string            `json:"runtime_root"`
	Environment            map[string]string `json:"environment"`
	WorkerAddress          string            `json:"worker_address"`
	HostdEndpoint          string            `json:"hostd_endpoint"`
	ControlEndpoint        string            `json:"control_endpoint"`
	HostdTokenFile         string            `json:"hostd_token_file"`
	DispatchCapabilityFile string            `json:"dispatch_capability_file"`
	LockPath               string            `json:"lock_path"`
}

func (c CarrierConfig) Validate() error {
	if c.Schema != carrierSchema || c.Source.Validate() != nil || c.Source.Platform != runtime.GOOS || c.Source.Architecture != runtime.GOARCH || c.Instance == "" || c.User == "" || c.Group == "" || !filepath.IsAbs(c.RuntimeRoot) || filepath.Clean(c.RuntimeRoot) != c.RuntimeRoot {
		return ErrInvalidRequest
	}
	host, port, err := net.SplitHostPort(c.WorkerAddress)
	if err != nil || port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return ErrInvalidRequest
	}
	if c.Environment["PAPERBOAT_RUNTIME_STATE_ROOT"] != c.RuntimeRoot || c.Environment["PAPERBOAT_RUNTIME_LISTEN_ADDRESS"] != c.WorkerAddress || c.Environment["PAPERBOAT_MACHINE_ID"] == "" {
		return ErrInvalidRequest
	}
	for key, value := range c.Environment {
		if strings.ContainsAny(key+value, "\x00\r\n") || strings.Contains(strings.ToUpper(key), "TOKEN") || strings.Contains(strings.ToUpper(key), "PASSWORD") || strings.Contains(strings.ToUpper(key), "SECRET") {
			return ErrInvalidRequest
		}
	}
	if c.Generation != c.Source.SHA256[:32] || c.ControlEndpoint != carrierControlEndpoint(c.HostdEndpoint, c.Generation) {
		return ErrInvalidRequest
	}
	return validateCarrierPaths(c)
}

// sourceGeneration names a source's per-generation units, sockets and locks: its
// SHA-256 prefix. The root helper refuses a malformed source instead of slicing it.
func sourceGeneration(s installsource.Source) (string, error) {
	if s.Validate() != nil {
		return "", ErrInvalidRequest
	}
	return s.SHA256[:32], nil
}

// carrierControlEndpoint is the account-private control socket of one carrier
// generation (from sourceGeneration); two generations serve side by side during an update.
func carrierControlEndpoint(hostdEndpoint, generation string) string {
	// A short generation suffix: macOS sun_path holds 104 bytes, and the full
	// 32-hex name made bind fail with EINVAL under /Library/Application Support
	// (pbmac: "initial carrier did not become ready"). 16 hex stay unique
	// among the few generations alive at once.
	return hostdEndpoint + ".c" + generation[:16]
}

func carrierInstaller(c CarrierConfig) (*service.Installer, error) {
	return carrierInstallerWithPending(c, false)
}
func carrierInstallerPending(c CarrierConfig) (*service.Installer, error) {
	return carrierInstallerWithPending(c, true)
}
func carrierInstallerWithPending(c CarrierConfig, pending bool) (*service.Installer, error) {
	controller, err := service.CarrierController(c.Source.Platform, c.Instance, c.Generation, c.UID, service.ExecRunner{})
	if err != nil {
		return nil, err
	}
	args := []string{"daemon", "__runtime-carrier", "--generation", c.Generation}
	if runtime.GOOS == "windows" {
		args = []string{"daemon", "__runtime-carrier", "--instance", c.Instance, "--generation", c.Generation}
	}
	config := service.Config{Platform: c.Source.Platform, Kind: service.CarrierKind, Instance: c.Instance, Generation: c.Generation, ConfigRoot: carrierServiceRoot(c), Executable: c.Executable, ExecutableSHA256: c.Source.SHA256, ExecutableLength: c.Source.Length, User: c.User, Group: c.Group, Arguments: args, Environment: c.Environment, UpgradeMode: service.UpgradeReload, Controller: controller, AccountReadableDefinition: c.Source.Platform != "windows"}
	if pending {
		return service.NewPending(config)
	}
	return service.New(config)
}

// PrepareCarrier publishes and starts only the initial carrier. Existing
// carriers are immutable here; replacement requires the separate drain gate.

func PrepareCarrier(ctx context.Context, request Request) (resultErr error) {
	if ctx == nil {
		return ErrInvalidRequest
	}
	operationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	lock, err := carrierPublicationLock(operationCtx, request)
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	}
	return prepareCarrierLocked(operationCtx, request)
}

// prepareCarrierLocked requires the existing native installation control lock.
func prepareCarrierLocked(ctx context.Context, request Request) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := prepareCarrierConfig(ctx, request)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	path := carrierConfigPath(c.Instance)
	previous, err := readCarrierConfigFile(ctx, c.Instance)
	if err == nil {
		want, _ := json.Marshal(c)
		have, _ := json.Marshal(previous)
		if string(want) != string(have) {
			return hostInstallFailure("existing carrier was preserved; replacement requires admission fencing and drained flows", ErrInvalidRequest)
		}
		installer, err := carrierInstaller(previous)
		if err != nil {
			return err
		}
		native := installerController(previous).(service.NativeLifecycleController)
		status, err := native.Inspect(ctx, installer.DefinitionPath())
		if err != nil {
			return err
		}
		if !status.Registered && !status.Running {
			if err := installer.PublishDefinition(ctx); err != nil {
				return err
			}
			if err := native.Apply(ctx, installer.DefinitionPath(), false); err != nil {
				return err
			}
			if err := waitCarrierReady(ctx, previous); err != nil {
				return cleanupCarrierStartup(ctx, installer, err)
			}
			return nil
		} else if err := installer.VerifyPublishedDefinition(); err != nil {
			return err
		}
		return waitCarrierReady(ctx, previous)
	}
	if !installNotExistOnly(err) {
		return err
	}
	installer, err := carrierInstaller(c)
	if err != nil {
		return err
	}
	native, ok := installerController(c).(service.NativeLifecycleController)
	if !ok {
		return ErrInvalidRequest
	}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return err
	}
	if status.Registered || status.Running {
		return ErrInvalidRequest
	}
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil {
		return err
	}
	if err := publishCarrierFiles(c, path, capability); err != nil {
		return err
	}
	if err := installer.Install(ctx); err != nil {
		return err
	}
	if err := waitCarrierReady(ctx, c); err != nil {
		return cleanupCarrierStartup(ctx, installer, err)
	}
	return nil
}

func installerController(c CarrierConfig) service.Controller {
	controller, _ := service.CarrierController(c.Source.Platform, c.Instance, c.Generation, c.UID, service.ExecRunner{})
	return controller
}

// EnsureExistingCarrier verifies retained custody without selecting candidate bytes.
func EnsureExistingCarrier(ctx context.Context, c CarrierConfig) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	retained, err := readCarrierConfigFile(ctx, c.Instance)
	if err != nil {
		return err
	}
	if retained.Validate() != nil {
		return ErrInvalidRequest
	}
	want, _ := json.Marshal(c)
	have, _ := json.Marshal(retained)
	if string(want) != string(have) {
		return ErrInvalidRequest
	}
	if err := retained.Source.Verify(retained.Executable); err != nil {
		return err
	}
	installer, err := carrierInstaller(retained)
	if err != nil {
		return err
	}
	native, ok := installerController(retained).(service.NativeLifecycleController)
	if !ok {
		return ErrInvalidRequest
	}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return err
	}
	switch carrierEnsureAction(status) {
	case ensureInstall:
		if err := installer.PublishDefinition(ctx); err != nil {
			return err
		}
		if err := native.Apply(ctx, installer.DefinitionPath(), false); err != nil {
			return err
		}
	case ensureStart:
		if err := native.Start(ctx, installer.DefinitionPath()); err != nil {
			return err
		}
	}
	if err := verifyCarrierNative(retained, installer); err != nil {
		return err
	}
	return waitCarrierReady(ctx, retained)
}

type ensureAction int

const (
	ensureNothing ensureAction = iota
	ensureInstall
	ensureStart
)

// carrierEnsureAction says what brings the active carrier back: install its definition when it has none, start
// it when it is declared but not running (stopped, or exited after an update released the network). Waiting for
// a carrier that nothing starts held the update lock for the whole wait on every reconcile pass and left the
// machine offline.
func carrierEnsureAction(status service.NativeControllerStatus) ensureAction {
	switch {
	case !status.Registered && !status.Running:
		return ensureInstall
	case !status.Running:
		return ensureStart
	}
	return ensureNothing
}

// ReadCarrierConfig accepts retained verified source pins and never consults
// privileged enrollment metadata or assumes the newest feature source.
// ReadCarrierDescriptor verifies protected declaration and source custody. It
// does not require the still-serving primary's native declaration to match a
// candidate that the installer may already have staged.
func ReadCarrierDescriptor(ctx context.Context, instance string) (CarrierConfig, error) {
	return verifyCarrierDescriptor(readCarrierConfigFile(ctx, instance))
}

func verifyCarrierDescriptor(c CarrierConfig, err error) (CarrierConfig, error) {
	if err != nil {
		return c, err
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	if err := c.Source.Verify(c.Executable); err != nil {
		return c, err
	}
	return c, nil
}

// ReadCarrierConfig reads the active carrier generation.
func ReadCarrierConfig(ctx context.Context, instance string) (CarrierConfig, error) {
	return verifyCarrierNativeConfig(ReadCarrierDescriptor(ctx, instance))
}

// ActiveCarrierGeneration names the account's active carrier generation; the
// account itself may read it. Its units are named after it (service.CarrierController).
func ActiveCarrierGeneration(ctx context.Context, instance string) (string, error) {
	c, err := ReadCarrierDescriptor(ctx, instance)
	return c.Generation, err
}

// ReadCarrierGeneration reads one generation's own descriptor, active or not: a
// carrier process serves the generation it was started as.
func ReadCarrierGeneration(ctx context.Context, instance, generation string) (CarrierConfig, error) {
	return verifyCarrierNativeConfig(verifyCarrierDescriptor(readCarrierGenerationFile(ctx, instance, generation)))
}

func verifyCarrierNativeConfig(c CarrierConfig, err error) (CarrierConfig, error) {
	if err != nil {
		return c, err
	}
	i, err := carrierInstaller(c)
	if err != nil {
		return c, err
	}
	if err := verifyCarrierNative(c, i); err != nil {
		return c, err
	}
	return c, nil
}

func decodeCarrier(body []byte) (CarrierConfig, error) {
	var c CarrierConfig
	err := decodeInstallJSON(strings.NewReader(string(body)), &c, 32<<10)
	return c, err
}

func ReadCarrierCapability(ctx context.Context, c CarrierConfig) ([]byte, error) {
	if ctx == nil || c.Validate() != nil {
		return nil, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openCarrierProtectedFile(c, c.DispatchCapabilityFile, false)
	if err != nil {
		return nil, err
	}
	b, readErr := io.ReadAll(io.LimitReader(f, 33))
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, ErrInvalidRequest
	}
	return b, nil
}

func jsonCarrier(c CarrierConfig) ([]byte, error) { return json.Marshal(c) }

// carrierReadyTimeout is how long a first carrier gets to answer its control pipe after its service starts: on a
// laptop with Defender scanning the 99 MB executable at first start, 15 s was not enough (victus, 2026-11-11,
// "initial carrier did not become ready" 31 s into a first pair).
const carrierReadyTimeout = 60 * time.Second

func waitCarrierReady(ctx context.Context, c CarrierConfig) error {
	readyCtx, cancel := context.WithTimeout(ctx, carrierReadyTimeout)
	defer cancel()
	capability, err := ReadCarrierCapability(readyCtx, c)
	if err != nil {
		return err
	}
	client, err := carriercontrol.NewClient(c.ControlEndpoint, capability, c.Source.SHA256, c.OwnerSID)
	if err != nil {
		return err
	}
	for {
		if err = client.Health(readyCtx); err == nil {
			return nil
		} else if !errors.Is(err, carriercontrol.ErrUnavailable) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return errors.Join(readyCtx.Err(), err)
		case <-timer.C:
		}
	}
}

// CarrierConfigPath derives the protected native descriptor path for the account.
func CarrierConfigPath(instance string) (string, error) {
	path := carrierConfigPath(instance)
	if path == "" {
		return "", ErrInvalidRequest
	}
	return path, nil
}

func cleanupCarrierStartup(ctx context.Context, i *service.Installer, cause error) error {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return hostInstallFailure("initial carrier did not become ready; published configuration retained for retry", cause, i.Uninstall(recovery))
}

type carrierUnlock func() error

func (unlock carrierUnlock) Close() error { return unlock() }

// ReconcileInstalledCarrier migrates a retained carrier only after its accepted
// SSH connections finish, under the existing native installation transaction.
func ReconcileInstalledCarrier(ctx context.Context, instance string, pinReferences ...func(context.Context, string) ([]string, error)) error {
	if ctx == nil || instance == "" || len(pinReferences) > 1 || len(pinReferences) == 1 && pinReferences[0] == nil {
		return ErrInvalidRequest
	}
	return reconcileInstalledCarrier(ctx, instance, pinReferences...)
}

// removeOtherCarrierGenerations removes every generation keep does not name and tries them all: the errors are joined,
// the first failure does not leave the remaining candidates behind.
func removeOtherCarrierGenerations(ctx context.Context, generations []CarrierConfig, keep func(CarrierConfig) bool, remove func(context.Context, CarrierConfig) error) error {
	var errs []error
	for _, c := range generations {
		if keep(c) {
			continue
		}
		if err := remove(ctx, c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
