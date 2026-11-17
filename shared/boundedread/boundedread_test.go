package boundedread

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestAll(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		limit   int64
		want    string
		wantErr error
	}{
		{"empty", "", 0, "", nil},
		{"under", "abc", 10, "abc", nil},
		{"exactly the limit", "abcd", 4, "abcd", nil},
		{"one over", "abcde", 4, "", ErrTooLarge},
		{"negative limit", "", -1, "", ErrTooLarge},
		{"max limit", "", 1<<63 - 1, "", ErrTooLarge},
		{"larger than the first buffer", strings.Repeat("x", firstBuffer+5), firstBuffer + 5, strings.Repeat("x", firstBuffer+5), nil},
		{"one over the first buffer", strings.Repeat("x", firstBuffer+1), firstBuffer, "", ErrTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := All(strings.NewReader(test.input), test.limit)
			if !errors.Is(err, test.wantErr) || string(got) != test.want {
				t.Fatalf("All() = %d bytes, %v; want %d bytes, %v", len(got), err, len(test.want), test.wantErr)
			}
		})
	}
}

func TestAllReadsAtMostLimitPlusOne(t *testing.T) {
	var source countingReader
	if _, err := All(&source, 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if source.read != 101 {
		t.Fatalf("read %d bytes, want 101", source.read)
	}
}

func TestAllPropagatesReadErrorAndShortReads(t *testing.T) {
	boom := errors.New("boom")
	if _, err := All(io.MultiReader(strings.NewReader("ab"), iotest.ErrReader(boom)), 10); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's error", err)
	}
	got, err := All(iotest.OneByteReader(bytes.NewReader([]byte("short reads"))), 100)
	if err != nil || string(got) != "short reads" {
		t.Fatalf("All() = %q, %v", got, err)
	}
}

type countingReader struct{ read int }

func (c *countingReader) Read(p []byte) (int, error) {
	c.read += len(p)
	return len(p), nil
}
