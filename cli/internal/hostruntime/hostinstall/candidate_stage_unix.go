//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

// candidateOutputLimit bounds what a candidate build reports back to the helper.
const candidateOutputLimit = 16 << 10

// runCandidate runs an operation of the candidate build's executable as root with
// the request on stdin and returns its stdout. Tests replace it.
var runCandidate = func(ctx context.Context, executable string, args []string, request Request) ([]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "PAPERBOAT_INVOKING_UID=" + strconv.Itoa(request.UID)}
	command.Stdin = bytes.NewReader(body)
	var stdout bytes.Buffer
	command.Stdout, command.Stderr = &stdout, os.Stderr
	// Run first: stdout.Bytes() in the same return statement is read before Run executes.
	err = command.Run()
	return stdout.Bytes(), err
}

// stageCandidateByExecutable has the candidate's own executable (its verified pin)
// write and start the candidate's units and descriptors (StageCandidate): the
// installing process may be another build. Rollback stays with the installing
// process, which removes them by generation name.
func stageCandidateByExecutable(ctx context.Context, request Request, carrier bool) (StagedCandidate, error) {
	pin, err := candidatePin(request)
	if err != nil {
		return StagedCandidate{}, err
	}
	return stageCandidateWith(ctx, pin, request, carrier)
}

func stageCandidateWith(ctx context.Context, pin string, request Request, carrier bool) (StagedCandidate, error) {
	args := []string{"__runtime-service", "stage-candidate"}
	if !carrier {
		args = append(args, "--session-owner-only")
	}
	out, err := runCandidate(ctx, pin, args, request)
	if err != nil {
		return StagedCandidate{}, hostInstallFailure("the new version could not start its own services", err)
	}
	var staged StagedCandidate
	// The candidate's report is another build's output: unknown fields are ignored.
	if err := json.NewDecoder(io.LimitReader(bytes.NewReader(out), candidateOutputLimit)).Decode(&staged); err != nil {
		return StagedCandidate{}, errors.Join(ErrInvalidRequest, err)
	}
	return staged, nil
}

// candidatePin is the candidate's protected, verified executable.
func candidatePin(request Request) (string, error) {
	paths := platformPaths(request.UID)
	pin := filepath.Join(filepath.Dir(paths.workerRollback), "pins", request.Source.SHA256, "pb")
	if _, err := os.Lstat(pin); installNotExistOnly(err) {
		if err := stageBinary(request.Executable, pin, request.Source); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return pin, request.Source.Verify(pin)
}

// CandidateRender is a build's own render of every unit an install or update
// writes for it: hostd (during an update and once committed), the updater, the
// privileged host service and the machine guard. The installing process may be
// another build, so it installs these bytes instead of rendering them itself.
type CandidateRender struct {
	HostdPending []byte            `json:"hostd_pending,omitempty"`
	Hostd        []byte            `json:"hostd,omitempty"`
	Updater      []byte            `json:"updater,omitempty"`
	Host         []byte            `json:"host,omitempty"`
	Guard        map[string]string `json:"guard"`
}

// renderOutputLimit bounds a candidate's render report (several units, base64).
const renderOutputLimit = 1 << 20

// RenderCandidate runs in the build being installed and renders its own units. A
// first install that is not enrolled yet writes only the machine guard.
func RenderCandidate(request Request) (CandidateRender, error) {
	render := CandidateRender{Guard: machineguard.Units()}
	if request.EnrollmentPending {
		return render, nil
	}
	paths := platformPaths(request.UID)
	for _, pending := range []bool{true, false} {
		hostd, updater, err := installersWithMaintenance(request, paths, true, pending, nil)
		if err != nil {
			return CandidateRender{}, err
		}
		body, err := hostd.Render()
		if err != nil {
			return CandidateRender{}, err
		}
		if !pending {
			render.Hostd = body
			if render.Updater, err = updater.Render(); err != nil {
				return CandidateRender{}, err
			}
			continue
		}
		render.HostdPending = body
	}
	host, err := hostInstallerWithMissing(request, paths, true)
	if err != nil {
		return CandidateRender{}, err
	}
	if render.Host, err = host.Render(); err != nil {
		return CandidateRender{}, err
	}
	return render, nil
}

func renderCandidateWith(ctx context.Context, executable string, request Request) (CandidateRender, error) {
	out, err := runCandidate(ctx, executable, []string{"__runtime-service", "render-candidate"}, request)
	if err != nil {
		return CandidateRender{}, hostInstallFailure("the new version could not render its own services", err)
	}
	var render CandidateRender
	if err := json.NewDecoder(io.LimitReader(bytes.NewReader(out), renderOutputLimit)).Decode(&render); err != nil {
		return CandidateRender{}, errors.Join(ErrInvalidRequest, err)
	}
	if len(render.Guard) == 0 || !request.EnrollmentPending && (len(render.Hostd) == 0 || len(render.HostdPending) == 0 || len(render.Updater) == 0 || len(render.Host) == 0) {
		return CandidateRender{}, ErrInvalidRequest
	}
	return render, nil
}

// candidateInstallers renders the candidate's units with its own executable and
// builds the installers that write them (privileged host, hostd, updater), plus the
// candidate's guard units.
func candidateInstallers(ctx context.Context, request Request, paths installPaths, maintenancePending bool) (host, hostd, updater *service.Installer, guard map[string]string, err error) {
	pin, err := candidatePin(request)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return candidateInstallersWith(ctx, pin, request, paths, maintenancePending)
}

func candidateInstallersWith(ctx context.Context, pin string, request Request, paths installPaths, maintenancePending bool) (host, hostd, updater *service.Installer, guard map[string]string, err error) {
	rendered, err := renderCandidateWith(ctx, pin, request)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if host, err = renderedHostInstaller(request, paths, true, rendered.Host); err != nil {
		return nil, nil, nil, nil, err
	}
	if hostd, updater, err = installersWithMaintenance(request, paths, true, maintenancePending, &rendered); err != nil {
		return nil, nil, nil, nil, err
	}
	return host, hostd, updater, rendered.Guard, nil
}
