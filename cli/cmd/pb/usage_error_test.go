package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUsageErrorShowsTheFailingCommandNotTheRootHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"access", "machine"}, "pb access machine <machine> [flags]"},
		{[]string{"access", "machine", "hp"}, "--port int"},
		{[]string{"edge", "bogus"}, "pb edge [flags]"},
		{[]string{"resolve"}, "pb resolve <query> [flags]"},
		{[]string{"approve"}, "pb approve <machine-id> [flags]"},
	} {
		var output bytes.Buffer
		code := run(context.Background(), append([]string{"--config", t.TempDir() + "/config.json"}, tc.args...), &output, &output)
		text := output.String()
		if code != 2 || !strings.Contains(text, "The command arguments are invalid.") || !strings.Contains(text, tc.want) || strings.Contains(text, "Available Commands:\n  access ") {
			t.Fatalf("%v: code=%d output=%q", tc.args, code, text)
		}
	}
}
