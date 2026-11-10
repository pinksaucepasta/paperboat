package session

import (
	"bytes"
	"io"
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

func TestConsoleFillReturnsAtOnceForAnEmptyBufferAndNeverReads(t *testing.T) {
	var decoder consoleDecoder
	reads := 0
	n, err := decoder.fill(nil, func() ([]uint16, error) { reads++; return []uint16{'a'}, nil })
	if n != 0 || err != nil || reads != 0 {
		t.Fatalf("fill(empty)=%d,%v after %d reads, want 0,nil,0", n, err, reads)
	}
}

func TestConsoleFillReadsAgainForTheSecondHalfOfAnEmoji(t *testing.T) {
	pair := utf16.Encode([]rune("😀"))
	reads := [][]uint16{{pair[0]}, {pair[1]}}
	var decoder consoleDecoder
	buffer := make([]byte, 16)
	n, err := decoder.fill(buffer, func() ([]uint16, error) {
		next := reads[0]
		reads = reads[1:]
		return next, nil
	})
	if err != nil || string(buffer[:n]) != "😀" || len(reads) != 0 {
		t.Fatalf("fill=%q,%v with %d reads left", buffer[:n], err, len(reads))
	}
}

func TestConsoleFillEndsAtAnEmptyRead(t *testing.T) {
	var decoder consoleDecoder
	if _, err := decoder.fill(make([]byte, 4), func() ([]uint16, error) { return nil, nil }); err != io.EOF {
		t.Fatalf("err=%v, want EOF", err)
	}
}
