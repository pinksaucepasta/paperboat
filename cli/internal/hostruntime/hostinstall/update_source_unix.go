//go:build darwin || linux

package hostinstall

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"os"
	"runtime"
)

// CommitUpdatedSource synchronizes installer ownership metadata with the exact
// payload authorized by the updater's durable committed transaction.
func CommitUpdatedSource(ctx context.Context, uid int, source installsource.Source) error {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	paths := platformPaths(uid)
	lock, err := LockSuppliedOperation(paths.metadata+".lock", 0)
	if err != nil {
		return err
	}
	defer lock.Close()
	request, err := loadInstallMetadata(paths.metadata, uid)
	if err != nil {
		return err
	}
	request, err = updatedRequestSource(request, paths.worker, source)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return writeInstallMetadata(paths.metadata, request)
}

func updatedRequestSource(request Request, binary string, source installsource.Source) (Request, error) {
	if source.Distribution != installsource.Official || source.Platform != runtime.GOOS || source.Architecture != runtime.GOARCH {
		return Request{}, ErrInvalidRequest
	}
	if err := source.Verify(binary); err != nil {
		return Request{}, err
	}
	source.AutomaticUpdates = request.Source.AutomaticUpdates
	request.Source = source
	request.Artifact.Version = source.Version
	request.Executable = binary
	return request, nil
}
