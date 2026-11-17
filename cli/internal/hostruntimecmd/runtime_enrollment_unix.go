//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

type runtimeEnrollmentCheckpointError struct{ cause error }

// currentInstallSource is the binary pairing installs as the runtime: this process's own, see liveExecutable.
func currentInstallSource() (string, installsource.Source, error) {
	path, err := liveExecutable(os.Executable, systemWorkerExecutable())
	if err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		return "", installsource.Source{}, err
	}
	source, err := installsource.Inspect(path, buildinfo.Version, buildinfo.Distribution)
	return path, source, err
}

// liveExecutable is this process's executable, or the installed runtime binary once that file is gone:
// `pb login --fresh` reinstalls pb and then pairs in the same process, and the reinstall moved the running
// binary to releases/pb.rollback and removed it (F13-02: ENOENT, "host runtime command failed").
func liveExecutable(running func() (string, error), installed string) (string, error) {
	path, err := running()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return installed, nil
	}
	return path, nil
}

func (e *runtimeEnrollmentCheckpointError) Error() string {
	return "persisting runtime enrollment progress failed; retry machine setup"
}

func (e *runtimeEnrollmentCheckpointError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// prepareUnixBootstrapRuntime validates the supplied binary before consuming a new
// runtime credential and checkpoints enrollment before service installation.
// If a process dies between Enroll and the checkpoint, the next run recognizes
// the matching durable identity and records progress without replaying the
// one-shot credential.
func prepareUnixBootstrapRuntime(ctx context.Context, material *bootstrap.Material, stateRoot string, client enrollmentClient, runtimeEnrolled bool, recordRuntime func() error) (string, error) {
	if material == nil || client == nil || recordRuntime == nil {
		return "", bootstrap.ErrInvalid
	}
	if material.ReuseIdentity {
		identity, err := enrollment.LoadRuntimeIdentityForRenewal(stateRoot, time.Now().UTC())
		if err != nil || !runtimeIdentityMatches(identity, *material) {
			return "", runtimeFailure("reusable runtime identity is unavailable or mismatched", bootstrap.ErrInvalid, err)
		}
	}
	artifactPath, _, err := currentInstallSource()
	if err != nil {
		return "", err
	}
	if !material.ReuseIdentity {
		identity, loadErr := enrollment.LoadRuntimeIdentityForRenewal(stateRoot, time.Now().UTC())
		if loadErr != nil && !runtimeCausesOnly(loadErr, os.ErrNotExist, enrollment.ErrInvalid) {
			return "", loadErr
		}
		if loadErr != nil || !runtimeIdentityMatches(identity, *material) {
			if _, err := client.Enroll(ctx, enrollment.Config{ControlURL: material.ControlURL, StateRoot: stateRoot, EnrollmentCredential: material.EnrollmentCredential}); err != nil {
				return "", err
			}
		}
	}
	if !runtimeEnrolled {
		if err := recordRuntime(); err != nil {
			return "", &runtimeEnrollmentCheckpointError{cause: err}
		}
	}
	if !material.ReuseIdentity {
		material.EnrollmentCredential = ""
	}
	return artifactPath, nil
}
