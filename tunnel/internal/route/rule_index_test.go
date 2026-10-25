package route

import (
	"context"
	"fmt"
	"testing"
)

// Audit T7: the per-generation host index gives the same choice (and the same conflicts) as
// scanning every rule, for exact, named and unnamed catch-all, wildcard and conflicting rules.
func TestRuleIndexChoosesLikeTheFullScan(t *testing.T) {
	rules := []RouteRule{
		matcherRule("catch", "", MatchCatchAll, "/", 1000),
		matcherRule("wild", "example.test", MatchOneLabelWildcard, "/", 100),
		matcherRule("nested-wild", "foo.example.test", MatchOneLabelWildcard, "/", 100),
		matcherRule("exact", "app.example.test", MatchExact, "/", 100),
		matcherRule("exact-api", "app.example.test", MatchExact, "/api", 100),
		matcherRule("named-catch", "named.example.test", MatchCatchAll, "/", 100),
		matcherRule("dup-a", "dup.example.test", MatchExact, "/", 100),
		matcherRule("dup-b", "dup.example.test", MatchExact, "/", 100),
	}
	index := newRuleIndex(rules)
	for _, host := range []string{"app.example.test", "x.example.test", "a.foo.example.test", "named.example.test", "dup.example.test", "other.test", "example.test"} {
		for _, path := range []string{"/", "/api/v1", "/static"} {
			want, wantErr := chooseRule(rules, host, path)
			got, gotErr := chooseRuleFrom(rules, index, host, path)
			if want.ID != got.ID || wantErr != gotErr {
				t.Fatalf("%s%s: indexed %q %v, full scan %q %v", host, path, got.ID, gotErr, want.ID, wantErr)
			}
		}
	}
}

// A stream open looks at the rules that can match its host, not all of them: flat from 10 to
// 4,096 rules.
func BenchmarkAcquireWithManyRules(b *testing.B) {
	for _, n := range []int{10, 4096} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			rules := make([]RouteRule, n)
			for i := range rules {
				rules[i] = matcherRule(fmt.Sprintf("rule-%d", i), fmt.Sprintf("host-%d.example.test", i), MatchExact, "/", 100)
			}
			registry := NewGenerationRegistry(n)
			if err := registry.StageGeneration(1, rules); err != nil {
				b.Fatal(err)
			}
			if err := registry.MarkGenerationReady(1); err != nil {
				b.Fatal(err)
			}
			if err := registry.ActivateGeneration(context.Background(), 1, 0); err != nil {
				b.Fatal(err)
			}
			host := fmt.Sprintf("host-%d.example.test", n/2)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lease, _, err := registry.Acquire(context.Background(), host, "/")
				if err != nil {
					b.Fatal(err)
				}
				_ = lease.Close()
			}
		})
	}
}
