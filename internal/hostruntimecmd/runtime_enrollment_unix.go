//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

type runtimeEnrollmentCheckpointError struct{ cause error }

var currentInstallSource = installsource.Current

func (e *runtimeEnrollmentCheckpointError) Error() string {
	return "persist runtime enrollment progress: " + e.cause.Error()
}

func (e *runtimeEnrollmentCheckpointError) Unwrap() error { return e.cause }

// prepareUnixBootstrapRuntime validates the supplied binary before consuming a new
// runtime credential and checkpoints enrollment before service installation.
// If a process dies between Enroll and the checkpoint, the next run recognizes
// the matching durable identity and records progress without replaying the
// one-shot credential.
func prepareUnixBootstrapRuntime(ctx context.Context, material *bootstrap.Material, stateRoot string, artifactHTTP *http.Client, client enrollmentClient, runtimeEnrolled bool, recordRuntime func() error) (string, error) {
	if material == nil || material.Artifact == nil || client == nil || recordRuntime == nil {
		return "", bootstrap.ErrInvalid
	}
	if material.ReuseIdentity {
		identity, err := enrollment.LoadRuntimeIdentityForRenewal(stateRoot, time.Now().UTC())
		if err != nil || !runtimeIdentityMatches(identity, *material) {
			return "", bootstrap.ErrInvalid
		}
	}
	artifactPath, _, err := currentInstallSource()
	if err != nil {
		return "", err
	}
	if !material.ReuseIdentity {
		identity, loadErr := enrollment.LoadRuntimeIdentityForRenewal(stateRoot, time.Now().UTC())
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
