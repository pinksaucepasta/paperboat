package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

type fakeMachineVersion struct {
	version string
	err     error
}

func (f fakeMachineVersion) MachineUpdateStatus(context.Context, string) (map[string]any, error) {
	return map[string]any{"current_version": f.version}, f.err
}

// X-36: an older target answers a newer client's stream header with a bare reset, which the daemon reports as "could not
// reach"; the failure names the machine's running pb and the fix instead.
func TestRefusedStreamNamesAnOlderMachine(t *testing.T) {
	previous := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = previous })
	machine := api.UserMachine{ID: "machine_1", Alias: "canary-t"}
	refusal := &localapi.RemoteError{StatusCode: 503, Code: "peer_transport_unavailable"}
	for name, c := range map[string]struct {
		ours, workspace string
		reader          fakeMachineVersion
		code            string
		want            []string
	}{
		"older release": {ours: "2026.11.16.2", reader: fakeMachineVersion{version: "2026.11.13.0"}, code: "peer_transport_unavailable", want: []string{"canary-t runs pb 2026.11.13.0, older than this pb 2026.11.16.2", "pb update"}},
		"dev builds":    {ours: "dev-6232eebec", reader: fakeMachineVersion{version: "dev-6e1e23255"}, code: "peer_stream_unavailable", want: []string{"canary-t runs pb dev-6e1e23255", "this pb is dev-6232eebec", "Update the older one"}},
		"same version":  {ours: "dev-a", reader: fakeMachineVersion{version: "dev-a"}, code: "peer_transport_unavailable"},
		"newer target":  {ours: "2026.11.13.0", reader: fakeMachineVersion{version: "2026.11.16.2"}, code: "peer_transport_unavailable"},
		"unreadable":    {ours: "dev-a", reader: fakeMachineVersion{err: errors.New("down")}, code: "peer_transport_unavailable"},
		"no version":    {ours: "dev-a", reader: fakeMachineVersion{}, code: "peer_transport_unavailable"},
		"exec start uncertain with a team workspace":    {ours: "dev-6232eebec", workspace: "team_1", reader: fakeMachineVersion{version: "dev-3c91531a7"}, code: "uncertain", want: []string{"canary-t runs pb dev-3c91531a7", "this pb is dev-6232eebec"}},
		"exec start uncertain in the default workspace": {ours: "dev-a", workspace: "personal", reader: fakeMachineVersion{version: "dev-b"}, code: "uncertain"},
		"another failure": {ours: "dev-a", reader: fakeMachineVersion{version: "dev-b"}, code: "exec_cwd_unavailable"},
	} {
		buildinfo.Version = c.ours
		err := error(&localapi.RemoteError{StatusCode: refusal.StatusCode, Code: c.code})
		if c.code == "uncertain" {
			err = fmt.Errorf("open: %w", localapi.ErrExecStartUncertain)
		}
		got := explainMachineVersionSkew(t.Context(), c.reader, machine, c.workspace, err)
		var skew *machineVersionSkewError
		if len(c.want) == 0 {
			if got != err {
				t.Errorf("%s: error replaced: %v", name, got)
			}
			continue
		}
		if !errors.As(got, &skew) || !errors.Is(got, err) {
			t.Fatalf("%s: not typed or lost its cause: %v", name, got)
		}
		message := commandFailureMessage(got, commandFailure{kind: commandOperational})
		for _, part := range c.want {
			if !strings.Contains(message, part) {
				t.Errorf("%s: %q lacks %q", name, message, part)
			}
		}
	}
}

// An older machine's refusal is permanent: the initial-connect loop must not retry it (the same wrapped
// peer_transport_unavailable otherwise counts as a transient transport failure), and a second look-up must not wrap it again.
func TestVersionSkewRefusalIsNotRetried(t *testing.T) {
	transient := &localapi.RemoteError{StatusCode: 503, Code: "peer_transport_unavailable"}
	if !retryableInitialConnectFailure(transient) {
		t.Fatal("the bare transport failure stopped being retryable")
	}
	skew := &machineVersionSkewError{message: "m runs pb a, older than this pb b; update it", cause: transient}
	if retryableInitialConnectFailure(skew) || retryableInitialConnectFailure(fmt.Errorf("dial: %w", skew)) {
		t.Fatal("a version-skew refusal is retried")
	}
	if got := explainMachineVersionSkew(t.Context(), fakeMachineVersion{version: "dev-x"}, api.UserMachine{ID: "m"}, "team_1", skew); got != error(skew) {
		t.Fatalf("an explained refusal was wrapped again: %v", got)
	}
}

// A named-workspace pb ssh to an older machine only sees the connection close: pb says why after OpenSSH exits.
func TestSSHClosedStreamNamesAnOlderMachine(t *testing.T) {
	previous := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = previous })
	buildinfo.Version = "dev-new"
	machine := api.UserMachine{ID: "machine_1", Alias: "canary-t"}
	reader := fakeMachineVersion{version: "dev-old"}
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetErr(&stderr)
	if explainSSHVersionSkew(command, reader, "personal", machine) || explainSSHVersionSkew(command, reader, "", machine) || stderr.Len() != 0 {
		t.Fatalf("a default workspace was explained as version skew: %q", stderr.String())
	}
	if !explainSSHVersionSkew(command, reader, "team_1", machine) || !strings.Contains(stderr.String(), "canary-t runs pb dev-old and this pb is dev-new") {
		t.Fatalf("named workspace: %q", stderr.String())
	}
	stderr.Reset()
	if explainSSHVersionSkew(command, fakeMachineVersion{version: "dev-new"}, "team_1", machine) || stderr.Len() != 0 {
		t.Fatalf("same version explained: %q", stderr.String())
	}
}
