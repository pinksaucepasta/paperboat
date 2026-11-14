package machineguard

import (
	"errors"
	"strings"
	"testing"
)

const (
	policyApple = "scrub-anchor \"com.apple/*\" all fragment reassemble\nanchor \"com.apple/*\" all\n"
	policyOwn   = "com.apple/000.paperboat-machineguard"
)

func TestDarwinPolicyRejectsBypasses(t *testing.T) {
	if hook, err := validateDarwinPolicy(policyApple, "lo0\n", "100.ApplicationFirewall\n", policyOwn, nil, nil); err != nil || hook {
		t.Fatalf("default policy: hook=%v err=%v", hook, err)
	}
	for _, test := range []struct{ root, interfaces, anchors string }{
		{"pass quick all\n" + policyApple, "lo0", ""},
		{"pass quick on lo0 all\n" + policyApple, "lo0", ""},
		{"anchor \"fw\" quick\n" + policyApple, "lo0", ""},
		{policyApple + policyApple, "lo0", ""},
		{policyApple, "lo0 (skip)\n", ""},
		{policyApple, "", ""},
		{policyApple, "lo0", "000.before-paperboat\n"},
	} {
		if _, err := validateDarwinPolicy(test.root, test.interfaces, test.anchors, policyOwn, nil, nil); err == nil {
			t.Fatalf("accepted unsafe policy %+v", test)
		}
	}
}

// TestDarwinPolicyAcceptsCustomRoots: a user's own pf.conf and third-party firewall anchors are normal. Rules
// after the Apple anchor cannot see protected traffic; non-quick rules and clean anchors before it cannot win.
func TestDarwinPolicyAcceptsCustomRoots(t *testing.T) {
	clean := func(string) error { return nil }
	for name, test := range map[string]struct {
		root     string
		wantHook bool
	}{
		"rules after the Apple anchor":     {policyApple + "block drop quick from 203.0.113.0/24 to any\npass quick on lo0 all\nanchor \"lulu\" all\n", false},
		"non-quick rule before":            {"block drop from 203.0.113.0/24 to any\n" + policyApple, false},
		"third-party anchor before":        {"anchor \"at.obdev.littlesnitch\" all\n" + policyApple, false},
		"no Apple anchor, user rules":      {"block drop quick from 203.0.113.0/24 to any\nanchor \"lulu\" all\n", true},
		"empty root ruleset":               {"", true},
		"root already hooked to the guard": {"anchor \"" + policyOwn + "\" all\nblock drop quick all\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			hook, err := validateDarwinPolicy(test.root, "lo0", "", policyOwn, clean, clean)
			if err != nil || hook != test.wantHook {
				t.Fatalf("hook=%v err=%v, want hook=%v", hook, err, test.wantHook)
			}
		})
	}
	if _, err := validateDarwinPolicy("anchor \"vpn\" all\n"+policyApple, "lo0", "", policyOwn, clean, func(string) error { return errors.New("quick") }); err == nil {
		t.Fatal("a root anchor holding quick rules before the Apple anchor was accepted")
	}
	if _, err := validateDarwinPolicy("pass quick on lo0 all\n"+policyApple, "lo0", "", policyOwn, clean, clean); err == nil || !strings.Contains(err.Error(), `"pass quick on lo0 all"`) {
		t.Fatalf("the bypassing rule is not named: %v", err)
	}
}

func TestDarwinInsertHookKeepsUserRules(t *testing.T) {
	got := darwinInsertHook("scrub-anchor \"x/*\" all\nblock drop quick from 203.0.113.0/24 to any\nanchor \"lulu\" all\n", policyOwn)
	want := "scrub-anchor \"x/*\" all\nanchor \"" + policyOwn + "\" all\nblock drop quick from 203.0.113.0/24 to any\nanchor \"lulu\" all\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := darwinInsertHook("", policyOwn); got != "anchor \""+policyOwn+"\" all\n" {
		t.Fatalf("empty root: %q", got)
	}
}

func TestDarwinRulesNoQuick(t *testing.T) {
	if darwinRulesNoQuick("block drop out proto tcp from any to any\n") != nil {
		t.Fatal("a rule without quick was refused")
	}
	if darwinRulesNoQuick("pass in quick all\n") == nil {
		t.Fatal("a quick rule was accepted")
	}
}

func TestDarwinEarlierAnchorRequiresEmptySubtree(t *testing.T) {
	for _, test := range []struct {
		name, rules, children, nested string
		queryError                    bool
		wantOK                        bool
	}{
		{name: "empty", wantOK: true},
		{name: "live", rules: "pass quick all"},
		{name: "nested-live", children: "com.apple/000.old/child", nested: "pass quick all"},
		{name: "nested-empty", children: "child", wantOK: true},
		{name: "unknown-child", children: "../foreign"},
		{name: "query-failure", queryError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := func(args ...string) (string, error) {
				if test.queryError {
					return "", errors.New("query failed")
				}
				if args[2] == "-sr" {
					if args[1] == "com.apple/000.old" {
						return test.rules, nil
					}
					return test.nested, nil
				}
				if args[1] == "com.apple/000.old" {
					return test.children, nil
				}
				return "", nil
			}
			budget := 64
			err := darwinEarlierAnchorOK(query, "com.apple/000.old", &budget, 0, darwinRulesEmpty)
			if (err == nil) != test.wantOK {
				t.Fatalf("err=%v wantOK=%v", err, test.wantOK)
			}
		})
	}
	root := "anchor \"com.apple/*\" all\n"
	if _, err := validateDarwinPolicy(root, "lo0", "000.old", "com.apple/100.guard", func(string) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDarwinPolicy(root, "lo0", "000.old", "com.apple/100.guard", func(string) error { return errors.New("not empty") }, nil); err == nil {
		t.Fatal("live earlier anchor accepted")
	}
}
