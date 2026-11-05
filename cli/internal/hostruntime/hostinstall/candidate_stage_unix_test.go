//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A signed update's helper is the previous build. It must have the candidate's
// own executable stage the candidate's units (pbvm1: the helper rendered the new
// carrier's unit with its own code and the new carrier refused it), and read the
// candidate's report as another build's output.
func TestSignedUpdateHasTheCandidateExecutableStageItsOwnUnits(t *testing.T) {
	previous := runCandidate
	t.Cleanup(func() { runCandidate = previous })
	request := Request{UID: 1001}
	var gotExecutable string
	var gotArgs []string
	var gotRequest Request
	report := `{"generation":"g","control_endpoint":"/run/x.cg","rendered_by":"a later build"}`
	runCandidate = func(_ context.Context, executable string, args []string, r Request) ([]byte, error) {
		gotExecutable, gotArgs, gotRequest = executable, args, r
		return []byte(report), nil
	}
	staged, err := stageCandidateWith(context.Background(), "/pins/candidate/pb", request, true)
	if err != nil || staged != (StagedCandidate{Generation: "g", ControlEndpoint: "/run/x.cg"}) {
		t.Fatalf("staged=%+v err=%v", staged, err)
	}
	if gotExecutable != "/pins/candidate/pb" || !slices.Equal(gotArgs, []string{"__runtime-service", "stage-candidate"}) || gotRequest != request {
		t.Fatalf("ran %s %v with %+v", gotExecutable, gotArgs, gotRequest)
	}
	if _, err = stageCandidateWith(context.Background(), "/pins/candidate/pb", request, false); err != nil || !slices.Equal(gotArgs, []string{"__runtime-service", "stage-candidate", "--session-owner-only"}) {
		t.Fatalf("session owner only: %v %v", gotArgs, err)
	}
	failed := errors.New("exit status 1")
	runCandidate = func(context.Context, string, []string, Request) ([]byte, error) { return nil, failed }
	if _, err = stageCandidateWith(context.Background(), "/pins/candidate/pb", request, true); !errors.Is(err, failed) {
		t.Fatalf("candidate failure lost: %v", err)
	}
}

// Every unit an install or update writes is the candidate build's own render,
// even when the installing process is another build (pbvm1: pb update --from ran
// the invoking CLI, which rendered the new build's units with its own code).
func TestEveryUnitTheUpdateWritesIsTheCandidatesOwnRender(t *testing.T) {
	previous := runCandidate
	t.Cleanup(func() { runCandidate = previous })
	request := validRequest(t)
	paths := platformPaths(request.UID)
	// What this build renders for the candidate when asked: the candidate's own render.
	own, err := RenderCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Guard) == 0 || len(own.Host) == 0 || len(own.Hostd) == 0 || len(own.HostdPending) == 0 || len(own.Updater) == 0 {
		t.Fatalf("render report misses a unit: %+v", own)
	}
	// A candidate build that renders every unit differently from the installing build.
	mark := func(b []byte) []byte { return append(append([]byte(nil), b...), "# only the candidate renders this\n"...) }
	candidate := CandidateRender{HostdPending: mark(own.HostdPending), Hostd: mark(own.Hostd), Updater: mark(own.Updater), Host: mark(own.Host), Guard: map[string]string{}}
	for name, unit := range own.Guard {
		candidate.Guard[name] = unit + "# only the candidate renders this\n"
	}
	runCandidate = func(_ context.Context, executable string, args []string, _ Request) ([]byte, error) {
		if executable != "/pins/candidate/pb" || !slices.Equal(args, []string{"__runtime-service", "render-candidate"}) {
			t.Fatalf("ran %s %v", executable, args)
		}
		return json.Marshal(candidate)
	}
	for _, pending := range []bool{true, false} {
		host, hostd, updater, guard, err := candidateInstallersWith(context.Background(), "/pins/candidate/pb", request, paths, pending)
		if err != nil {
			t.Fatal(err)
		}
		wantHostd := candidate.Hostd
		if pending {
			wantHostd = candidate.HostdPending
		}
		for name, check := range map[string]struct {
			installer interface{ Render() ([]byte, error) }
			want      []byte
		}{"host": {host, candidate.Host}, "hostd": {hostd, wantHostd}, "updater": {updater, candidate.Updater}} {
			if got, err := check.installer.Render(); err != nil || !bytes.Equal(got, check.want) {
				t.Fatalf("pending=%v: the %s unit written is not the candidate's render: %v", pending, name, err)
			}
		}
		if len(guard) != len(candidate.Guard) {
			t.Fatalf("guard units %v", guard)
		}
		for name, unit := range candidate.Guard {
			if guard[name] != unit {
				t.Fatalf("guard unit %s is not the candidate's render", name)
			}
		}
	}
	runCandidate = func(context.Context, string, []string, Request) ([]byte, error) {
		return json.Marshal(CandidateRender{Guard: candidate.Guard, Hostd: []byte("final\n")})
	}
	if _, _, _, _, err = candidateInstallersWith(context.Background(), "/pins/candidate/pb", request, paths, true); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a render report missing units was accepted: %v", err)
	}
}

// The real path: the installing process runs another executable (the candidate)
// and reads its report from stdout. pbvm1: A2's own install failed with
// "invalid privileged installation request; EOF" because runCandidate read the
// candidate's stdout before running it. A script stands in for a candidate build
// that renders every unit differently.
func TestInstallRunsTheCandidateExecutableAndInstallsItsReport(t *testing.T) {
	request := validRequest(t)
	paths := platformPaths(request.UID)
	own, err := RenderCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	mark := func(b []byte) []byte { return append(append([]byte(nil), b...), "# rendered by the candidate executable\n"...) }
	report := CandidateRender{HostdPending: mark(own.HostdPending), Hostd: mark(own.Hostd), Updater: mark(own.Updater), Host: mark(own.Host), Guard: map[string]string{}}
	for name, unit := range own.Guard {
		report.Guard[name] = unit + "# rendered by the candidate executable\n"
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "report.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	// The candidate reads the request (as `pb __runtime-service render-candidate` does) and prints its report.
	candidate := filepath.Join(dir, "pb")
	script := "#!/bin/sh\n[ \"$1 $2\" = \"__runtime-service render-candidate\" ] || exit 9\ncat >/dev/null\nexec cat " + filepath.Join(dir, "report.json") + "\n"
	if err = os.WriteFile(candidate, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	host, hostd, updater, guard, err := candidateInstallersWith(context.Background(), candidate, request, paths, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]struct {
		installer interface{ Render() ([]byte, error) }
		want      []byte
	}{"host": {host, report.Host}, "hostd": {hostd, report.HostdPending}, "updater": {updater, report.Updater}} {
		if got, err := check.installer.Render(); err != nil || !bytes.Equal(got, check.want) {
			t.Fatalf("the %s unit written is not the candidate executable's render: %v", name, err)
		}
	}
	for name, unit := range report.Guard {
		if guard[name] != unit {
			t.Fatalf("guard unit %s is not the candidate executable's render", name)
		}
	}
}
