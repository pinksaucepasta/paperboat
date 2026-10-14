//go:build linux || darwin || windows

package hostruntimecmd

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

func sessionOwnerInvocation(args []string, stderr io.Writer) (string, string, error) {
	flags := flag.NewFlagSet("session-owner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	generation := flags.String("generation", "", "private store generation")
	instance := flags.String("instance", "", "enrolled account instance")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !sessionowner.ValidGeneration(*generation) || (runtime.GOOS == "windows") != (*instance != "") {
		return "", "", sessionowner.ErrInvalid
	}
	return *generation, *instance, nil
}

func readSessionOwnerDescriptor(root, generation string) (sessionowner.Descriptor, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !sessionowner.ValidGeneration(generation) {
		return sessionowner.Descriptor{}, sessionowner.ErrInvalid
	}
	d, err := sessionowner.Read(filepath.Join(root, generation+".json"))
	if err != nil {
		return sessionowner.Descriptor{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return sessionowner.Descriptor{}, err
	}
	if d.Generation != generation || d.Source.Platform != runtime.GOOS || d.Source.Architecture != runtime.GOARCH || filepath.Clean(executable) != d.Executable {
		return sessionowner.Descriptor{}, sessionowner.ErrInvalid
	}
	if err := d.Source.Verify(executable); err != nil {
		return sessionowner.Descriptor{}, err
	}
	return d, nil
}

func runIndependentSessions(ctx context.Context, d sessionowner.Descriptor, catalogRoot string, ready func() error, watchdog func() error, interval time.Duration) (resultErr error) {
	owner, err := hostruntime.NewIndependentSessionOwner(ctx, d, catalogRoot)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, owner.Close(closeCtx))
	}()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- owner.Run(runCtx) }()
	defer func() { cancel(); resultErr = errors.Join(resultErr, <-done) }()
	select {
	case err := <-owner.Ready():
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ready(); err != nil {
		return err
	}
	var ticks <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			// Return the listener result through the deferred join exactly once.
			done <- err
			if err == nil {
				return sessionowner.ErrInvalid
			}
			return err
		case <-ticks:
			if err := watchdog(); err != nil {
				return err
			}
		}
	}
}
