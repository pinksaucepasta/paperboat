package configsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMergeRegularText(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tests := []struct {
		name   string
		base   string
		local  string
		remote string
		want   string
		clean  bool
	}{
		{name: "non-overlapping", base: "one\ntwo\nthree\n", local: "ONE\ntwo\nthree\n", remote: "one\ntwo\nTHREE\n", want: "ONE\ntwo\nTHREE\n", clean: true},
		{name: "adjacent replacements", base: "a\nb\n", local: "A\nb\n", remote: "a\nB\n", want: "A\nB\n", clean: true},
		{name: "independent insertions", base: "a\nb\nc\n", local: "x\na\nb\nc\n", remote: "a\nb\nc\ny\n", want: "x\na\nb\nc\ny\n", clean: true},
		{name: "independent deletions", base: "a\nb\nc\nd\n", local: "b\nc\nd\n", remote: "a\nb\nc\n", want: "b\nc\n", clean: true},
		{name: "insertion and replacement", base: "a\nb\nc\n", local: "x\na\nb\nc\n", remote: "a\nb\nC\n", want: "x\na\nb\nC\n", clean: true},
		{name: "identical edits plus independent change", base: "a\nb\nc\nd\n", local: "A\nb\nc\nD\n", remote: "A\nb\nc\nd\n", want: "A\nb\nc\nD\n", clean: true},
		{name: "identical sides", base: "a\n", local: "b\n", remote: "b\n", want: "b\n", clean: true},
		{name: "unchanged local", base: "a\n", local: "a\n", remote: "b\n", want: "b\n", clean: true},
		{name: "unchanged remote", base: "a\n", local: "b\n", remote: "a\n", want: "b\n", clean: true},
		{name: "empty base identical insert", local: "a\n", remote: "a\n", want: "a\n", clean: true},
		{name: "empty base different insert", local: "a\n", remote: "b\n"},
		{name: "both delete", base: "a\n", clean: true},
		{name: "same position insert", base: "a\n", local: "x\na\n", remote: "y\na\n"},
		{name: "insert inside deletion", base: "a\nb\nc\nd\n", local: "a\nd\n", remote: "a\nb\nx\nc\nd\n"},
		{name: "insert at deletion start", base: "a\nb\nc\n", local: "a\nc\n", remote: "a\nx\nb\nc\n"},
		{name: "insert at deletion end", base: "a\nb\nc\n", local: "a\nc\n", remote: "a\nb\nx\nc\n"},
		{name: "delete versus modify", base: "a\nb\nc\n", local: "a\nc\n", remote: "a\nB\nc\n"},
		{name: "repeated lines", base: "a\nx\nx\nx\nz\n", local: "A\nx\nx\nx\nz\n", remote: "a\nx\nx\nx\nZ\n", want: "A\nx\nx\nx\nZ\n", clean: true},
		{name: "CRLF", base: "a\r\nb\r\nc\r\n", local: "A\r\nb\r\nc\r\n", remote: "a\r\nb\r\nC\r\n", want: "A\r\nb\r\nC\r\n", clean: true},
		{name: "unterminated final line", base: "a\nb\nc", local: "A\nb\nc", remote: "a\nb\nC", want: "A\nb\nC", clean: true},
		{name: "add final newline", base: "a\nb\nc", local: "A\nb\nc", remote: "a\nb\nc\n", want: "A\nb\nc\n", clean: true},
		{name: "remove final newline", base: "a\nb\nc\n", local: "A\nb\nc\n", remote: "a\nb\nc", want: "A\nb\nc", clean: true},
		{name: "invalid UTF8", base: "a\n", local: "\xff\n", remote: "b\n"},
		{name: "overlapping", base: "one\n", local: "local\n", remote: "remote\n"},
		{name: "binary", base: "one\x00", local: "local\x00", remote: "remote\x00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, mergeErr := mergeRegularText(context.Background(), []byte(test.base), []byte(test.local), []byte(test.remote), 1<<20)
			if test.clean {
				if mergeErr != nil || string(got) != test.want {
					t.Fatalf("merge = %q, %v", got, mergeErr)
				}
				return
			}
			if !errors.Is(mergeErr, ErrTextMergeConflict) || got != nil {
				t.Fatalf("error = %v", mergeErr)
			}
		})
	}
}

func TestMergeRegularTextHonorsCancellationAndOutputBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mergeRegularText(ctx, []byte("a\n"), []byte("b\n"), []byte("a\n"), 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if _, err := mergeRegularText(context.Background(), []byte("a\n"), []byte("long output\n"), []byte("a\n"), 2); !errors.Is(err, ErrTextMergeConflict) {
		t.Fatalf("bound error = %v", err)
	}
}

func TestMergeRegularTextMergedOutputBound(t *testing.T) {
	// Each input fits, but the combined independent insertions do not.
	got, err := mergeRegularText(context.Background(), []byte("a\nb\nc\n"), []byte("left\na\nb\nc\n"), []byte("a\nb\nc\nright\n"), 12)
	if !errors.Is(err, ErrTextMergeConflict) || got != nil {
		t.Fatalf("overflow returned %q, %v", got, err)
	}
}

func TestMergeRegularTextWorkBound(t *testing.T) {
	var base, local, remote strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&base, "base%d\n", i)
		fmt.Fprintf(&local, "local%d\n", i)
		fmt.Fprintf(&remote, "remote%d\n", i)
	}
	got, err := mergeRegularText(context.Background(), []byte(base.String()), []byte(local.String()), []byte(remote.String()), 1<<20)
	if !errors.Is(err, ErrTextMergeConflict) || got != nil {
		t.Fatalf("over-budget merge returned %q, %v", got, err)
	}
	// A large unchanged span with small, distant edits remains inexpensive.
	middle := strings.Repeat("unchanged\n", 10000)
	got, err = mergeRegularText(context.Background(), []byte("a\n"+middle+"z\n"), []byte("A\n"+middle+"z\n"), []byte("a\n"+middle+"Z\n"), 1<<20)
	if err != nil || string(got) != "A\n"+middle+"Z\n" {
		t.Fatalf("sparse merge failed: %v", err)
	}
}

type cancelDuringMergeContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringMergeContext) Err() error {
	c.checks++
	if c.checks == 400 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestMergeRegularTextCancelsDuringDiff(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelDuringMergeContext{Context: parent, cancel: cancel}
	got, err := mergeRegularText(ctx, []byte(strings.Repeat("base\n", 100)), []byte(strings.Repeat("local\n", 100)), []byte(strings.Repeat("remote\n", 100)), 1<<20)
	if !errors.Is(err, context.Canceled) || got != nil || ctx.checks < 400 {
		t.Fatalf("during-diff cancellation: %v, checks %d", err, ctx.checks)
	}
}

func FuzzMergeRegularText(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), []byte("A\nb\nc\n"), []byte("a\nb\nC\n"))
	f.Add([]byte("a\na\nb\n"), []byte("a\nb\n"), []byte("a\na\nx\nb\n"))
	f.Add([]byte(""), []byte("x"), []byte("y"))
	f.Fuzz(func(t *testing.T, base, local, remote []byte) {
		if len(base) > 512 || len(local) > 512 || len(remote) > 512 {
			t.Skip()
		}
		budget := &mergeBudget{ctx: context.Background(), remaining: mergeWorkLimit}
		edits, err := lineEdits(budget, base, local)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt, err := renderEdits(budget, base, 0, len(base), edits, 2048)
		if err != nil || !bytes.Equal(rebuilt, local) {
			t.Fatalf("diff did not reconstruct side: %v", err)
		}
		got, err := mergeRegularText(context.Background(), base, local, remote, 2048)
		reversed, reverseErr := mergeRegularText(context.Background(), base, remote, local, 2048)
		if (err == nil) != (reverseErr == nil) || !bytes.Equal(got, reversed) {
			t.Fatal("merge is not symmetric")
		}
		if err != nil {
			if got != nil || !errors.Is(err, ErrTextMergeConflict) {
				t.Fatal("invalid conflict outcome")
			}
			return
		}
		if !mergeText(got) || len(got) > 2048 {
			t.Fatal("invalid clean merge")
		}
	})
}
