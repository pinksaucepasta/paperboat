package main

import (
	"bytes"
	"context"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"strings"
	"testing"
)

func TestTerminalSharingRejectsAmbiguousAudienceAndRole(t *testing.T) {
	for _, flags := range [][]string{{"--team", "team_1"}, {"--team", "team_1", "--all", "--member", "usr_2"}, {"--team", "team_1", "--all", "--role", "owner"}} {
		var out bytes.Buffer
		args := append([]string{"session", "share", "ses_1"}, flags...)
		if code := run(context.Background(), args, &out, &out); code != 2 || !strings.Contains(out.String(), "share requires") {
			t.Fatalf("code=%d output=%s", code, out.String())
		}
	}
}

// Task40 CLI compilation consumes the declared unsigned flag added by the
// concurrent Inbox task. Verify its value survives the existing command bridge.
func TestTerminalSharingBuildUnsignedFlagBridge(t *testing.T) {
	for _, value := range []string{"7", "-1"} {
		called := false
		tree := specTree(&command.Spec{Name: "fixture", Subcommands: []*command.Spec{{Name: "read", Flags: []command.Flag{&command.UintFlag{Name: "generation"}}, Action: func(c *command.Context) error {
			called = true
			if c.Uint("generation") != 7 {
				t.Fatal("generation lost")
			}
			return nil
		}}}}, "fixture")
		var output bytes.Buffer
		tree.SetOut(&output)
		tree.SetErr(&output)
		tree.SetArgs([]string{"read", "--generation", value})
		err := tree.ExecuteContext(context.Background())
		if value == "7" && (err != nil || !called) {
			t.Fatalf("unsigned bridge: %v", err)
		}
		if value == "-1" && (err == nil || called) {
			t.Fatal("negative generation reached action")
		}
	}
}
