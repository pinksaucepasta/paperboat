package session

import "testing"

func TestRemotePasteFollowsTheRemoteProgram(t *testing.T) {
	const on, vt = bracketedPasteEnableSequence, win32InputDisableSequence
	for _, tc := range []struct {
		output, shown string
		enabled       bool
	}{
		{"bash-3.2$ ", "bash-3.2$ ", false},
		{"\x1b[?2004h$ ", "\x1b[?2004h$ ", true},
		{"\x1b[?2004h$ \x1b[?2004l\r\nout", "\x1b[?2004h$ \x1b[?2004l" + on + "\r\nout", false},
		{"\x1b[?2004l\x1b[?2004h", "\x1b[?2004l" + on + "\x1b[?2004h", true},
		{"\x1b[?1049;2004h", "\x1b[?1049;2004h", true},
		{"\x1b[?2004h\x1b[?1049;2004lx", "\x1b[?2004h\x1b[?1049;2004l" + on + "x", false},
		{"\x1b[?2004h\x1bcx", "\x1b[?2004h\x1bc" + on + "x", false},
		{"\x1b[?2004h\x1b[?12004l\x1b[2004l\x1b[?25l\x1b(c\x1b]0;?2004l\x07", "\x1b[?2004h\x1b[?12004l\x1b[2004l\x1b[?25l\x1b(c\x1b]0;?2004l\x07", true},
		// What a Windows target's ConPTY sends first: the local console must not take up win32-input-mode.
		{"\x1b[?9001h\x1b[?1004hC:\\>", "\x1b[?9001h" + vt + "\x1b[?1004hC:\\>", false},
		{"\x1b[?1004;9001hx", "\x1b[?1004;9001h" + vt + "x", false},
		{"\x1b[?9001;2004l\x1b[?9001l", "\x1b[?9001;2004l" + on + "\x1b[?9001l", false},
		{"\x1b[?2004;9001h", "\x1b[?2004;9001h" + vt, true},
		{"\x1b[?19001h\x1b[9001h", "\x1b[?19001h\x1b[9001h", false},
	} {
		// Whole, and split between any two reads.
		for split := 0; split <= len(tc.output); split++ {
			remote := &RemotePaste{}
			shown := string(remote.follow([]byte(tc.output[:split])))
			shown += string(remote.follow([]byte(tc.output[split:])))
			if shown != tc.shown || remote.Enabled() != tc.enabled {
				t.Fatalf("follow(%q split at %d) = %q, enabled %v; want %q, %v", tc.output, split, shown, remote.Enabled(), tc.shown, tc.enabled)
			}
		}
	}
}

// A shared-terminal join frames nothing: only the local console's input encoding is held.
func TestUnframedOutputKeepsOnlyThePlainInputEncoding(t *testing.T) {
	remote := &RemotePaste{unframed: true}
	const output = "\x1b[?9001h\x1b[?2004h\x1b[?2004l\x1bc"
	if got, want := string(remote.follow([]byte(output))), "\x1b[?9001h"+win32InputDisableSequence+output[len("\x1b[?9001h"):]; got != want {
		t.Fatalf("follow(%q) = %q, want %q", output, got, want)
	}
}
