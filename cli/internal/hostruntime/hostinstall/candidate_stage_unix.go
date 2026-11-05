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
	return stdout.Bytes(), command.Run()
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

// CandidateRender is a build's own render of its hostd unit: during an update
// (maintenance pending) and once the update commits.
type CandidateRender struct {
	HostdPending []byte `json:"hostd_pending"`
	Hostd        []byte `json:"hostd"`
}

// renderOutputLimit bounds a candidate's render report (two units, base64).
const renderOutputLimit = 512 << 10

// RenderCandidate runs in the build being installed and renders its own hostd unit.
func RenderCandidate(request Request) (CandidateRender, error) {
	paths := platformPaths(request.UID)
	var render CandidateRender
	for _, pending := range []bool{true, false} {
		hostd, _, err := installersWithMaintenance(request, paths, true, pending, nil)
		if err != nil {
			return CandidateRender{}, err
		}
		body, err := hostd.Render()
		if err != nil {
			return CandidateRender{}, err
		}
		if pending {
			render.HostdPending = body
		} else {
			render.Hostd = body
		}
	}
	return render, nil
}

func renderCandidateByExecutable(ctx context.Context, request Request) (CandidateRender, error) {
	pin, err := candidatePin(request)
	if err != nil {
		return CandidateRender{}, err
	}
	return renderCandidateWith(ctx, pin, request)
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
	if len(render.Hostd) == 0 || len(render.HostdPending) == 0 {
		return CandidateRender{}, ErrInvalidRequest
	}
	return render, nil
}
