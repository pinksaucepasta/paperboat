//go:build linux || darwin || windows

package hostinstall

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// ResolveUpdateBundle must run in the trusted privileged installer with the
// existing native update lock held. Receiver state is derived from native layout.
func ResolveUpdateBundle(ctx context.Context, request Request, updateState string) (Request, func(), error) {
	noop := func() {}
	if request.UpdateBundle == "" {
		return request, noop, nil
	}
	if !filepath.IsAbs(request.UpdateBundle) || filepath.Clean(request.UpdateBundle) != request.UpdateBundle {
		return Request{}, noop, ErrInvalidRequest
	}
	if err := secureUpdateState(updateState, request); err != nil {
		return Request{}, noop, err
	}
	staging, err := os.MkdirTemp(updateState, ".supplied-bundle-")
	if err != nil {
		return Request{}, noop, err
	}
	cleanup := func() { os.RemoveAll(staging) }
	repository, err := buildinfo.ReleaseRepository()
	if err != nil {
		cleanup()
		return Request{}, noop, err
	}
	verified, err := bootstrap.ImportUpdateBundle(ctx, repository, filepath.Join(updateState, "tuf", "index"), request.UpdateBundle, staging, time.Now())
	if err != nil {
		cleanup()
		if bundleProofFailure(err) {
			return Request{}, noop, ErrUpdateVerificationFailed
		}
		return Request{}, noop, err
	}
	if !verified.Index.AvailableForNewInstall(time.Now()) {
		cleanup()
		return Request{}, noop, ErrUpdateVerificationFailed
	}
	executable := verified.ArtifactPath

	source, err := installsource.InspectBuild(executable, verified.Index.Version)
	if err != nil {
		cleanup()
		return Request{}, noop, errors.Join(bootstrap.ErrArtifactMismatch, err)
	}
	source.Distribution = installsource.Official
	source.AutomaticUpdates = true
	request.Executable, request.Source = executable, source
	request.Artifact = bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1, Kind: bootstrap.ArtifactKindPB, Version: source.Version, Platform: source.Platform, Architecture: source.Architecture, RepositoryURL: repository, TargetPath: verified.Index.Targets[0].TargetPath}
	request.UpdateBundle = ""
	return request, cleanup, nil
}

// PrepareSuppliedMaintenance protects admitted mutations and ordinary protected
// workloads before an enrolled installation stops any service. Force is explicit.
func PrepareSuppliedMaintenance(ctx context.Context, socket, tokenFile string, force bool) (func(), error) {
	abort, err := prepareMaintenance(ctx, socket, tokenFile, force, 35*time.Second)
	if err != nil {
		return nil, err
	}
	return func() { _ = abort() }, nil
}

func prepareMaintenance(ctx context.Context, socket, tokenFile string, force bool, timeout time.Duration) (func() error, error) {
	info, err := os.Lstat(tokenFile)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 32 {
		return nil, ErrInvalidRequest
	}
	file, err := os.Open(tokenFile)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, ErrInvalidRequest
	}
	token, readErr := io.ReadAll(io.LimitReader(file, 33))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(token) != 32 {
		return nil, ErrInvalidRequest
	}
	client, err := hostdproto.NewClient(socket, token, timeout)
	if err != nil {
		return nil, err
	}
	if err = client.PrepareMaintenance(ctx, force); err != nil {
		return nil, err
	}
	return func() error {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return client.AbortMaintenance(recovery)
	}, nil
}

func bundleProofFailure(err error) bool {
	for n := 0; err != nil && n < 16; n++ {
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		if err == bootstrap.ErrArtifactMismatch || err == bootstrap.ErrArtifactTarget || err == tar.ErrHeader || err == io.ErrUnexpectedEOF {
			return true
		}
		switch err.(type) {
		case *metadata.ErrRepository, *metadata.ErrUnsignedMetadata, *metadata.ErrBadVersionNumber, *metadata.ErrEqualVersionNumber, *metadata.ErrExpiredMetadata, *metadata.ErrLengthOrHashMismatch, *metadata.ErrDownloadLengthMismatch, *metadata.ErrDownloadHTTP, *metadata.ErrValue, *metadata.ErrType:
			return true
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
