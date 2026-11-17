package redact

import (
	"regexp"
	"testing"
)

func TestApplyCountsMatchesAndKeepsCapturedPrefix(t *testing.T) {
	rules := []Rule{
		{Pattern: regexp.MustCompile(`(token=)\S+`), Replacement: `${1}[X]`},
		{Pattern: regexp.MustCompile(`\d{3}`), Replacement: `[N]`},
	}
	got, count := Apply("token=abc token=def 123 4567", rules)
	if got != "token=[X] token=[X] [N] [N]7" || count != 4 {
		t.Fatalf("Apply() = %q, %d", got, count)
	}
}

func TestApplyOrderMatters(t *testing.T) {
	aToB := Rule{Pattern: regexp.MustCompile(`a`), Replacement: "b"}
	bToC := Rule{Pattern: regexp.MustCompile(`b`), Replacement: "c"}
	if got, count := Apply("a", []Rule{aToB, bToC}); got != "c" || count != 2 {
		t.Fatalf("a->b then b->c = %q, %d", got, count)
	}
	if got, count := Apply("a", []Rule{bToC, aToB}); got != "b" || count != 1 {
		t.Fatalf("b->c then a->b = %q, %d", got, count)
	}
}

func TestApplyNoRulesOrNoMatchIsUnchanged(t *testing.T) {
	if got, count := Apply("plain", nil); got != "plain" || count != 0 {
		t.Fatalf("Apply(nil) = %q, %d", got, count)
	}
	if got, count := Apply("plain", []Rule{{Pattern: regexp.MustCompile(`zzz`), Replacement: "x"}}); got != "plain" || count != 0 {
		t.Fatalf("Apply(no match) = %q, %d", got, count)
	}
}
