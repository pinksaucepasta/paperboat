//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// A source upgrade starts the new carrier generation in its own unit and waits
// until it serves while the old generation keeps serving everything. The update
// then marks the new generation active and, once it commits, retires the old
// one, which finishes its open streams and exits (make-before-break). Prior art:
// Teleport graceful restart (lib/service/signals.go) and nginx binary upgrade
// (~/ref/nginx/src/os/unix/ngx_process_cycle.c). Nothing is refused meanwhile.
type unixCarrierUpgrade struct {
	candidate StagedCandidate
	previous  CarrierConfig
	terminals *terminalHandover
}

// StagedCandidate is what a build reports after staging its own generation: what
// the previous build's helper needs to move clients to it and mark it active.
type StagedCandidate struct {
	Generation      string `json:"generation"`
	ControlEndpoint string `json:"control_endpoint"`
}

// stageUnixUpgrade has the build being installed stage its own session owner and,
// for a source upgrade of a serving carrier, its own carrier generation (stage);
// then it moves the old generation's clients to it. Each build renders and checks
// only its own units: a signed update's helper is the previous build, so it runs
// the candidate's executable for this (stageCandidateByExecutable).
func stageUnixUpgrade(ctx context.Context, request Request, paths installPaths, carrier bool, stage func(context.Context, bool) (StagedCandidate, error)) (_ *unixCarrierUpgrade, resultErr error) {
	var retained CarrierConfig
	if carrier {
		var err error
		if retained, err = ReadCarrierConfig(ctx, paths.instance); err != nil {
			return nil, err
		}
		carrier = retained.Source != request.Source
	}
	staged, err := stage(ctx, carrier)
	if err != nil || !carrier {
		return nil, err
	}
	generation, err := sourceGeneration(request.Source)
	if err != nil || staged.Generation != generation || !filepath.IsAbs(staged.ControlEndpoint) || filepath.Clean(staged.ControlEndpoint) != staged.ControlEndpoint {
		return nil, errors.Join(ErrInvalidRequest, err)
	}
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_ready")
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return nil, err
	}
	defer clear(capability)
	client, err := carriercontrol.NewClient(retained.ControlEndpoint, capability, retained.Source.SHA256)
	if err != nil {
		return nil, err
	}
	// Every connected client opens its new connections on the candidate from
	// here; open streams stay on the old generation until they close. A client
	// that declines stays on the old one until it retires.
	moveErr := moveCarrierClients(ctx, staged.ControlEndpoint, request.Source.SHA256, capability, client)
	diagnosticlog.TryInfo("carrier cutover", "stage", "move_clients", "failed", moveErr != nil)
	terminals, moveErr := stageTerminalHandover(ctx, request, retained, client)
	if moveErr != nil {
		diagnosticlog.TryWarn("terminal handover unavailable", "fallback", "reconnect")
	}
	return &unixCarrierUpgrade{candidate: staged, previous: retained, terminals: terminals}, nil
}

// StageCandidate runs in the build being installed, as root: it starts its own
// session owner and, with carrier, its own carrier generation next to the serving
// one, rendered by this build, and waits until that generation serves (A source
// upgrade starts the new generation in its own unit while the old one keeps
// serving everything; the update marks it active and retires the old one once it
// commits: make-before-break. Prior art: Teleport graceful restart,
// lib/service/signals.go, and nginx binary upgrade,
// ~/ref/nginx/src/os/unix/ngx_process_cycle.c.)
func StageCandidate(ctx context.Context, request Request, carrier bool) (StagedCandidate, error) {
	if ctx == nil {
		return StagedCandidate{}, ErrInvalidRequest
	}
	if os.Geteuid() != 0 {
		return StagedCandidate{}, ErrNotPrivileged
	}
	if err := Validate(request, request.UID); err != nil {
		return StagedCandidate{}, err
	}
	if err := PrepareSessionOwner(ctx, request); err != nil || !carrier {
		return StagedCandidate{}, err
	}
	paths := platformPaths(request.UID)
	// The serving generation's unit is the previous build's render: only its descriptor is read here.
	retained, err := verifyCarrierDescriptor(readCarrierConfigFile(ctx, paths.instance))
	if err != nil {
		return StagedCandidate{}, err
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return StagedCandidate{}, err
	}
	defer clear(capability)
	candidate, err := prepareCarrierConfig(ctx, request)
	if err != nil {
		return StagedCandidate{}, err
	}
	// Endpoint and account custody cannot change during a source upgrade.
	candidate.Environment = retained.Environment
	stable := candidate
	stable.Source, stable.Generation, stable.Executable = retained.Source, retained.Generation, retained.Executable
	stable.ControlEndpoint, stable.LockPath = retained.ControlEndpoint, retained.LockPath
	want, _ := json.Marshal(stable)
	have, _ := json.Marshal(retained)
	if !bytes.Equal(want, have) {
		return StagedCandidate{}, ErrInvalidRequest
	}
	// Failures past this point are undone by the lifecycle rollback, which
	// removes the candidate generation by name (unixCarrierInstallation.Restore).
	if err := publishCarrierFiles(candidate, "", capability); err != nil {
		return StagedCandidate{}, err
	}
	next, err := carrierInstaller(candidate)
	if err != nil {
		return StagedCandidate{}, err
	}
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_start")
	if err := next.Install(ctx); err != nil {
		return StagedCandidate{}, err
	}
	if err := waitCarrierReady(ctx, candidate); err != nil {
		return StagedCandidate{}, err
	}
	return StagedCandidate{Generation: candidate.Generation, ControlEndpoint: candidate.ControlEndpoint}, nil
}

// moveCarrierClients has the serving generation offer the candidate's signed
// generation offer to every client connected to it (Owner.OfferCarrier).
func moveCarrierClients(ctx context.Context, endpoint, sha256 string, capability []byte, previous *carriercontrol.Client) error {
	next, err := carriercontrol.NewClient(endpoint, capability, sha256)
	if err != nil {
		return err
	}
	offer, err := next.GenerationOffer(ctx)
	if err != nil {
		return err
	}
	return previous.MoveTerminals(ctx, offer)
}

// activate marks the already serving candidate generation active: readers of
// the account's carrier descriptor address it from now on.
func (u *unixCarrierUpgrade) activate() error {
	if u == nil {
		return nil
	}
	err := promoteCarrierGeneration(u.previous, u.candidate.Generation)
	diagnosticlog.TryInfo("carrier cutover", "stage", "candidate_active", "failed", err != nil)
	return err
}

// promoteCarrierGeneration makes a generation's own descriptor the account's
// active one, byte for byte: the candidate build wrote it, and the previous
// build's helper must not re-encode it with its own idea of the descriptor.
func promoteCarrierGeneration(previous CarrierConfig, generation string) error {
	f, err := openCarrierProtectedFile(previous, carrierGenerationPath(previous.Instance, generation), false)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(f, carrierDescriptorLimit+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > carrierDescriptorLimit {
		return ErrInvalidRequest
	}
	return atomicfile.Write(carrierConfigPath(previous.Instance), body, atomicfile.Options{Mode: 0640, OwnerUID: 0, OwnerGID: previous.GID})
}

// finish retires the previous generation once the update committed. A failed
// update was already rolled back to it, so it keeps serving untouched.
func (u *unixCarrierUpgrade) finish(ctx context.Context, failed bool) error {
	if u == nil {
		return nil
	}
	err := u.terminals.finish(ctx, failed)
	if failed {
		return err
	}
	// The update is committed; reconciliation retries a retirement that fails here.
	if _, retireErr := retireCarrierGeneration(context.WithoutCancel(ctx), u.previous); retireErr != nil {
		diagnosticlog.TryWarn("carrier retirement deferred", "stage", "retire_previous")
	}
	return err
}

// retireCarrierGeneration stops a non-active generation from starting at boot and
// asks its running process to finish its open streams and exit (service.QuitCarrier).
// It reports whether the process still runs. Repeating it is harmless.
func retireCarrierGeneration(ctx context.Context, c CarrierConfig) (bool, error) {
	installer, err := carrierInstallerPending(c)
	if err != nil {
		return false, err
	}
	controller := installerController(c)
	native, ok := controller.(service.NativeLifecycleController)
	if !ok {
		return false, service.ErrInvalidDefinition
	}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return false, err
	}
	// launchd's disable is a bootout, which would cut open streams; a retired
	// generation started at boot exits on the next reconciliation's signal.
	if status.Enabled && runtime.GOOS == "linux" {
		if err := native.Disable(ctx, installer.DefinitionPath()); err != nil {
			return status.Running, err
		}
	}
	if !status.Running {
		return false, nil
	}
	return true, service.QuitCarrier(ctx, controller)
}
