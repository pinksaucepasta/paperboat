package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

func TestManagedSSHLaunchRepairsOnceThenRefuses(t *testing.T) {
	broken := managedssh.ErrOpenSSHConfigConflict
	for _, test := range []struct {
		name      string
		results   []error
		refresh   error
		refreshes int
		want      string
	}{
		{"ready", []error{nil}, nil, 0, ""},
		{"repaired", []error{broken, nil}, nil, 1, ""},
		{"still broken", []error{broken, broken}, nil, 1, "SSH was not started"},
		{"no daemon", []error{broken}, localapi.ErrTransportUnavailable, 1, "pb status"},
		{"options", []error{managedssh.ErrOpenSSHLaunchUnpinned}, nil, 0, "host-key check"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, refreshes := 0, 0
			ctx := withHostDependencies(context.Background(), func(edges *hostDependencies) {
				edges.verifySSHLaunch = func(context.Context, managedssh.OpenSSHLaunch) error { calls++; return test.results[calls-1] }
				edges.refreshInventory = func(context.Context) error { refreshes++; return test.refresh }
			})
			err := verifyManagedSSHLaunch(ctx, managedssh.Destination{Host: "box.example.test"}, "ssh", nil, nil)
			if (err == nil) != (test.want == "") || refreshes != test.refreshes || calls != len(test.results) {
				t.Fatalf("err=%v refreshes=%d calls=%d", err, refreshes, calls)
			}
			if err != nil && !strings.Contains(userFacingError(err), test.want) {
				t.Fatalf("message=%q, want %q", userFacingError(err), test.want)
			}
		})
	}
}

func TestManagedToolSSHOptions(t *testing.T) {
	for _, test := range []struct {
		tool        string
		args        []string
		environment []string
		want        []string
		refused     bool
	}{
		{"scp", []string{"-rF", "x", "-P", "22", "-oA=b", "-i", "-F", "f", "box:/x"}, nil, []string{"-F", "x", "-o", "A=b"}, false},
		{"scp", []string{"-S", "/tmp/other", "f", "box:/x"}, nil, nil, true},
		{"scp", []string{"f", "-J", "hop", "box:/x"}, nil, []string{"-J", "hop"}, false},
		{"sftp", []string{"-b", "-F", "-o", "A=b", "box"}, nil, []string{"-o", "A=b"}, false},
		{"rsync", []string{"-a", "f", "box:/x"}, nil, nil, false},
		{"rsync", []string{"-ae", "ssh -F x", "f", "box:/x"}, nil, []string{"-F", "x"}, false},
		{"rsync", []string{"--rsh=ssh -o A=b", "f", "box:/x"}, nil, []string{"-o", "A=b"}, false},
		{"rsync", []string{"f", "box:/x"}, []string{"RSYNC_RSH=/tmp/evil"}, nil, true},
		{"rsync", []string{"-e", "nc", "f", "box:/x"}, nil, nil, true},
		{"rsync", []string{"-e", "ssh -o 'A=b c'", "f", "box:/x"}, nil, nil, true},
	} {
		_, options, err := managedToolSSH(test.tool, test.args, test.environment)
		if (err != nil) != test.refused || len(options)+len(test.want) > 0 && !reflect.DeepEqual(options, test.want) {
			t.Errorf("%s %q: options=%q err=%v", test.tool, test.args, options, err)
		}
	}
	var calls []string
	_, err := rewriteManagedToolArguments(managedToolTestContext(), "rsync", []string{"f", "mac::module"}, managedToolTestDomain, managedToolTestResolver(&calls), nil)
	if !errors.Is(err, errManagedToolOperand) {
		t.Errorf("rsync daemon operand: err=%v", err)
	}
}
