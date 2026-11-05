package tunnel

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

// F3-15 N4: a probe link attaches from the cursor of its dial while the stalled link it replaces still delivers
// output; the replayed part came back as duplicate lines. An adopted link drops what the terminal already has.
func TestLocalPeerConnDropsReplayedOutputAnotherLinkDelivered(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	connection, err := NewLocalPeerConn(client, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	delivered := 6 // the stalled link delivered through sequence 6 while this link attached from 4
	skip, ok := connection.(interface{ SkipDelivered(func() int) })
	if !ok {
		t.Fatal("a local peer link cannot skip output another link delivered")
	}
	skip.SkipDelivered(func() int { return delivered })
	go func() {
		writer := &localPeerWriter{writer: peer}
		for _, frame := range []struct {
			sequence uint64
			text     string
		}{{5, "five\n"}, {6, "six\n"}, {7, "seven\n"}} {
			payload := binary.BigEndian.AppendUint64(nil, frame.sequence)
			_ = writer.write(localPeerTerminalData, append(payload, frame.text...))
		}
	}()
	got := make([]byte, len("seven\n"))
	if _, err := io.ReadFull(connection, got); err != nil || string(got) != "seven\n" {
		t.Fatalf("first output after adoption = %q (%v), want only what the other link had not delivered", got, err)
	}
}

// F3-15 N13: the probe link replays from the cursor of its dial as ONE frame (bytes 100..160) while the stalled link
// delivered through 130 after the network came back. The frame is not wholly delivered, so dropping whole frames left
// the 30 bytes the terminal already had (66 duplicated lines after a 60 s black hole). A frame that continues the
// link's own byte cursor is trimmed to what the terminal lacks; a frame that does not (a screen checkpoint) is never cut.
func TestLocalPeerConnTrimsAReplayFrameThatStraddlesWhatAnotherLinkDelivered(t *testing.T) {
	for name, tc := range map[string]struct {
		baseline uint64
		want     string
	}{
		"continues the cursor": {100, strings.Repeat("B", 30)},
		"does not continue it": {90, strings.Repeat("A", 30) + strings.Repeat("B", 30)},
	} {
		t.Run(name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			connection, err := NewLocalPeerConn(client, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			connection.(interface{ SkipDelivered(func() int) }).SkipDelivered(func() int { return 130 })
			go func() {
				writer := &localPeerWriter{writer: peer}
				_ = writer.write(localPeerTerminalSequence, binary.BigEndian.AppendUint64(nil, tc.baseline))
				_ = writer.write(localPeerTerminalData, append(binary.BigEndian.AppendUint64(nil, 160), strings.Repeat("A", 30)+strings.Repeat("B", 30)...))
			}()
			got := make([]byte, len(tc.want))
			if _, err := io.ReadFull(connection, got); err != nil || string(got) != tc.want {
				t.Fatalf("output after adoption = %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}
