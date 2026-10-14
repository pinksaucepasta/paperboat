//go:build linux || darwin

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func carrierInstance(_ string) (string, error) { return "u" + strconv.Itoa(os.Geteuid()), nil }
func carrierHostdToken(config hostinstall.CarrierConfig) ([]byte, error) {
	return readWorkerToken(config.HostdTokenFile)
}

func runNativeCarrier(ctx context.Context, config hostinstall.CarrierConfig) (resultErr error) {
	if config.UID != os.Geteuid() || config.GID != os.Getegid() {
		return hostinstall.ErrInvalidRequest
	}
	notifier, err := service.NewProcessNotifier()
	if err != nil {
		return err
	}
	if err := notifier.Starting(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, notifier.Stopping()) }()
	return runCarrierProcess(ctx, config, notifier.Ready, notifier.Watchdog, notifier.WatchdogInterval())
}
