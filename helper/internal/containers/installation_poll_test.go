package containers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func pollFixture(inspectErrs, probeErrs int) (func(context.Context, string) (containerInfo, bool, error), map[string]func(context.Context) (NodeReadiness, error)) {
	inspect := func(context.Context, string) (containerInfo, bool, error) {
		if inspectErrs > 0 {
			inspectErrs--
			return containerInfo{}, false, errors.New("docker busy")
		}
		return containerInfo{Owner: "me", Running: true, ImageID: "img"}, true, nil
	}
	probe := func(context.Context) (NodeReadiness, error) {
		if probeErrs != 0 {
			if probeErrs > 0 {
				probeErrs--
			}
			return NodeReadiness{}, errors.New("connection reset")
		}
		return NodeReadiness{Ready: true, ProcessEpoch: "new"}, nil
	}
	return inspect, map[string]func(context.Context) (NodeReadiness, error){"edge": probe}
}

func TestPollInstallationReadySurvivesTransientErrors(t *testing.T) {
	inspect, probes := pollFixture(2, 2)
	err := pollInstallationReady(context.Background(), "me", map[string]string{"edge": "img"}, map[string]string{"edge": "old"}, inspect, probes)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPollInstallationReadyTimeoutNamesLastFailure(t *testing.T) {
	inspect, probes := pollFixture(0, -1)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := pollInstallationReady(ctx, "me", map[string]string{"edge": "img"}, nil, inspect, probes)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("got %v", err)
	}
}

func TestPollInstallationReadyForeignContainerFailsAtOnce(t *testing.T) {
	_, probes := pollFixture(0, 0)
	inspect := func(context.Context, string) (containerInfo, bool, error) {
		return containerInfo{Owner: "other", Running: true}, true, nil
	}
	if err := pollInstallationReady(context.Background(), "me", nil, nil, inspect, probes); !errors.Is(err, errForeignContainer) {
		t.Fatalf("got %v", err)
	}
}
