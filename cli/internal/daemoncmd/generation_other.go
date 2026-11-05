//go:build !darwin && !linux

package daemoncmd

import (
	"context"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// Daemon generations drain side by side only on Linux and macOS; Windows keeps one service.
func notifyQuit() <-chan os.Signal { return nil }
func takeDaemonLock(context.Context, localapi.Paths) (func(localdaemon.DaemonConfig) localdaemon.DaemonConfig, error) {
	return func(config localdaemon.DaemonConfig) localdaemon.DaemonConfig { return config }, nil
}
func runLiveGeneration(context.Context, localapi.Paths, string) {}
func publishDrain(localapi.Paths, string) func(int, time.Time) {
	return func(int, time.Time) {}
}
func withdrawGenerationState(localapi.Paths, string) {}
