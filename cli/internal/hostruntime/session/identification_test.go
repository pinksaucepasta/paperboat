package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIdentificationFragmentation(t *testing.T) {
	stream := "hello\x1b]0;✳ API debugging\x07\x1b]7;file://machine/projects/my%20api\x1b\\world\x1b]1;ignored\x07"
	for split := 0; split <= len(stream); split++ {
		var tracker identificationTracker
		tracker.Consume([]byte(stream[:split]))
		tracker.Consume([]byte(stream[split:]))
		if tracker.title != "✳ API debugging" || tracker.directory != "/projects/my api" {
			t.Fatalf("split %d: title=%q directory=%q", split, tracker.title, tracker.directory)
		}
	}
	var tracker identificationTracker
	for _, b := range []byte(stream) {
		tracker.Consume([]byte{b})
	}
	if tracker.title != "✳ API debugging" || tracker.directory != "/projects/my api" {
		t.Fatal("bytewise metadata differs")
	}
	tracker.Consume([]byte("\x1b]2;\x1b\\"))
	if tracker.title != "" {
		t.Fatal("empty title did not clear application title")
	}
}

func TestIdentificationBoundsAndRecovery(t *testing.T) {
	var tracker identificationTracker
	tracker.Consume([]byte("\x1b]2;original\x07"))
	tracker.Consume([]byte("\x1b]2;" + strings.Repeat("x", maxIdentificationSequence+100)))
	if len(tracker.payload) > maxIdentificationSequence {
		t.Fatal("unbounded parser")
	}
	tracker.Consume([]byte("\x07\x1b]2;\xff\x07"))
	if tracker.title != "original" {
		t.Fatal("invalid or oversized OSC replaced title")
	}
	tracker.Consume([]byte("\x1b]2;" + strings.Repeat("界", 200) + "\x07"))
	if utf8.RuneCountInString(tracker.title) != 128 {
		t.Fatal("title Unicode bound")
	}
	tracker.Consume([]byte("\x1b]2;abc\n\t\u202edef\x07"))
	if tracker.title != "abcdef" {
		t.Fatalf("unsafe metadata %q", tracker.title)
	}
	tracker.Consume([]byte("\x1b]2;bad\x18\x1b]2;recovered\x07"))
	if tracker.title != "recovered" {
		t.Fatal("cancelled OSC prevents recovery")
	}
}

func TestIdentificationRejectsOtherControlStringsAndInvalidDirectories(t *testing.T) {
	for _, prefix := range []string{"\x1bP", "\x1b_", "\x1b^", "\x1bX"} {
		var tracker identificationTracker
		tracker.Consume([]byte(prefix + "\x1b]2;fake\x07\x1b\\"))
		if tracker.title != "" {
			t.Fatal("nested OSC inside control string accepted")
		}
		tracker.Consume([]byte("\x1b]2;real\x07"))
		if tracker.title != "real" {
			t.Fatal("control string does not recover")
		}
	}
	for _, path := range []string{"https://host/work", "file:relative", "file://user@host/work", "file://host:42/work", "file:///work?secret=x", "file:///work#x", "file:///bad%zz"} {
		var tracker identificationTracker
		tracker.Consume([]byte("\x1b]7;file:///real\x07\x1b]7;" + path + "\x07"))
		if tracker.directory != "/real" {
			t.Fatalf("invalid URI %q changed directory", path)
		}
	}
}

func FuzzIdentification(f *testing.F) {
	f.Add([]byte("\x1b]2;hello\x07\x1b]7;file:///work\x1b\\"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var tracker identificationTracker
		for offset := 0; offset < len(data); {
			end := min(offset+17, len(data))
			tracker.Consume(data[offset:end])
			offset = end
			if len(tracker.payload) > maxIdentificationSequence {
				t.Fatal("parser bound")
			}
		}
		if utf8.RuneCountInString(tracker.title) > 128 || utf8.RuneCountInString(tracker.directory) > 1024 {
			t.Fatal("metadata bound")
		}
	})
}
