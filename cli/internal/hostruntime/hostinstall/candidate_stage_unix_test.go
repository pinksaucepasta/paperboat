//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// The helper writes the candidate's hostd unit through its journaled lifecycle,
// with the candidate's own render: the bytes it installs and verifies are the
// candidate's, not this build's render of the candidate.
func TestSignedUpdateInstallsTheCandidatesOwnHostdRender(t *testing.T) {
	previous := runCandidate
	t.Cleanup(func() { runCandidate = previous })
	request := validRequest(t)
	paths := platformPaths(request.UID)
	own, _, err := installersWithMaintenance(request, paths, true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownRender, err := own.Render()
	if err != nil {
		t.Fatal(err)
	}
	candidate := append(append([]byte(nil), ownRender...), "# a directive only the candidate renders\n"...)
	runCandidate = func(_ context.Context, _ string, args []string, _ Request) ([]byte, error) {
		if !slices.Equal(args, []string{"__runtime-service", "render-candidate"}) {
			t.Fatalf("args %v", args)
		}
		return json.Marshal(CandidateRender{HostdPending: candidate, Hostd: []byte("final\n")})
	}
	rendered, err := renderCandidateWith(context.Background(), "/pins/candidate/pb", request)
	if err != nil {
		t.Fatal(err)
	}
	hostd, _, err := installersWithMaintenance(request, paths, true, true, rendered.HostdPending)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := hostd.Render(); err != nil || !bytes.Equal(got, candidate) {
		t.Fatalf("installed hostd unit is not the candidate's render: %v", err)
	}
	runCandidate = func(context.Context, string, []string, Request) ([]byte, error) {
		return json.Marshal(CandidateRender{Hostd: []byte("final\n")})
	}
	if _, err = renderCandidateWith(context.Background(), "/pins/candidate/pb", request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a render without the update-time unit was accepted: %v", err)
	}
}
