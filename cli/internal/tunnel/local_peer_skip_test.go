package tunnel

import (
	"encoding/binary"
	"io"
	"net"
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
