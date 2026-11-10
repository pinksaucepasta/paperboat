package session

import (
	"bytes"
	"testing"
	"unicode/utf16"
)

func TestConsoleDecoderKeepsASplitEmojiWhole(t *testing.T) {
	pair := utf16.Encode([]rune("😀")) // D83D DE00
	var decoder consoleDecoder
	buffer := make([]byte, 64)
	if n := decoder.decode(buffer, append([]uint16{'a'}, pair[0])); n != 1 || buffer[0] != 'a' {
		t.Fatalf("first read gave %q, want only the a (the high half is held back)", buffer[:n])
	}
	if n := decoder.decode(buffer, append([]uint16{pair[1]}, 'b')); !bytes.Equal(buffer[:n], []byte("😀b")) {
		t.Fatalf("second read gave % x, want the whole emoji then b", buffer[:n])
	}
}

func TestConsoleDecoderHandsOutWhatDidNotFit(t *testing.T) {
	var decoder consoleDecoder
	var got []byte
	small := make([]byte, 1) // smaller than one UTF-8 sequence: nothing is torn, it is just delivered over several calls
	first := decoder.decode(small, utf16.Encode([]rune("é😀")))
	got = append(got, small[:first]...)
	for decoder.pending() {
		n := decoder.decode(small, nil)
		got = append(got, small[:n]...)
	}
	if string(got) != "é😀" {
		t.Fatalf("got % x", got)
	}
}

func TestConsoleDecoderPassesControlBytesAndVTUntouched(t *testing.T) {
	var decoder consoleDecoder
	buffer := make([]byte, 64)
	input := "\x1b[A\x1b[200~x\x03\x1a\x1b[201~"
	n := decoder.decode(buffer, utf16.Encode([]rune(input)))
	if string(buffer[:n]) != input {
		t.Fatalf("got %q", buffer[:n])
	}
}
