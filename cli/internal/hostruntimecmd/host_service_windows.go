//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	hostserviceinstance "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

// ExecuteHostService runs the legacy privileged availability endpoint as a
// native named-pipe server. New installations use hostd, but keeping this
// entry real is required for idempotent repair of existing installations.
func ExecuteHostService(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("pb daemon __runtime-host-service", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pb: invalid host-service invocation")
		return 2
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		fmt.Fprintln(stderr, "pb: invalid Windows user")
		return 1
	}
	instance, err := hostserviceinstance.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	instanceRoot, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	socketPath, err := hostservice.WindowsSocketPath(instance)
	if err != nil {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	applier := hostservice.NewPlatformApplier(filepath.Join(instanceRoot, "power-baseline.json"))
	authorizedKeys, err := hostservice.NewWindowsAuthorizedKeys(instance)
	if err != nil {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	server, err := hostservice.New(hostservice.Config{SocketPath: socketPath, StatePath: filepath.Join(instanceRoot, "availability-policy.json"), SID: config.OwnerSID, Applier: applier, Version: buildinfo.Version, AuthorizedKeys: authorizedKeys})
	if err != nil {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "pb:", err)
		return 1
	}
	return 0
}
