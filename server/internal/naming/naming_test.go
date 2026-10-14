package naming

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestVocabularyIsExactly192DisjointSimpleWords(t *testing.T) {
	seen := map[string]bool{}
	for _, list := range [][]string{qualities[:], atmospheres[:], waypoints[:]} {
		if len(list) != 64 {
			t.Fatalf("list length = %d", len(list))
		}
		for _, word := range list {
			if seen[word] {
				t.Fatalf("repeated word %q", word)
			}
			seen[word] = true
			for _, c := range word {
				if c < 'a' || c > 'z' {
					t.Fatalf("invalid word %q", word)
				}
			}
			if len(word) > 12 || len(word) < 2 {
				t.Fatalf("word length %q", word)
			}
		}
	}
	if len(seen) != 192 {
		t.Fatalf("unique vocabulary = %d", len(seen))
	}
}

func TestRandomNamesUseAllPositionsAndPreserveFourDigits(t *testing.T) {
	for i := 0; i < 64; i++ {
		got, err := Session(bytes.NewReader([]byte{byte(i), byte(i), byte(i)}))
		want := qualities[i] + "-" + atmospheres[i] + "-" + waypoints[i]
		if err != nil || got != want {
			t.Fatalf("position %d: %q %v, want %q", i, got, err, want)
		}
	}
	for _, digits := range []struct {
		input  []byte
		suffix string
	}{
		{[]byte{0, 0}, "0000"}, {[]byte{0, 7}, "0007"}, {[]byte{0x27, 0x0f}, "9999"},
		// 16383 is rejected rather than biased by modulo arithmetic.
		{[]byte{0xff, 0xff, 0, 7}, "0007"},
	} {
		got, err := PublicSlug(bytes.NewReader(append([]byte{0, 1, 2}, digits.input...)))
		if err != nil || got != "agile-amber-barn-"+digits.suffix {
			t.Fatalf("slug %q %v", got, err)
		}
	}
}

func TestRandomFailureReturnsNoPartialName(t *testing.T) {
	for _, input := range [][]byte{nil, {0}, {0, 0}, {0, 0, 0}, {0, 0, 0, 0}} {
		got, err := PublicSlug(bytes.NewReader(input))
		if err == nil || got != "" || (!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
			t.Fatalf("failure = %q %v", got, err)
		}
	}
}

func TestDefaultRandomGeneratorIsSafeForConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := PublicSlug(nil)
			if err != nil {
				t.Error(err)
				return
			}
			parts := strings.Split(got, "-")
			if len(parts) != 4 || len(parts[3]) != 4 || len(got) > 63 {
				t.Errorf("invalid slug %q", got)
			}
		}()
	}
	wg.Wait()
}
