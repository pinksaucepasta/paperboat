//go:build darwin

package deviceguard

import (
	"errors"
	"testing"
)

func TestDarwinPolicyRejectsBypasses(t *testing.T) {
	root := "scrub-anchor \"com.apple/*\" all fragment reassemble\nanchor \"com.apple/*\" all\n"
	own := "com.apple/000.paperboat-deviceguard"
	if err := validateDarwinPolicy(root, "lo0\n", "100.ApplicationFirewall\n", own); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ root, interfaces, anchors string }{
		{"pass quick all\n" + root, "lo0", ""},
		{root + "pass quick all\n", "lo0", ""},
		{root, "lo0 (skip)\n", ""},
		{root, "", ""},
		{root, "lo0", "000.before-paperboat\n"},
		{"anchor \"other/*\" all\n", "lo0", ""},
	} {
		if err := validateDarwinPolicy(test.root, test.interfaces, test.anchors, own); err == nil {
			t.Fatalf("accepted unsafe policy %+v", test)
		}
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
			err := darwinEarlierAnchorEmpty(query, "com.apple/000.old", &budget, 0)
			if (err == nil) != test.wantOK {
				t.Fatalf("err=%v wantOK=%v", err, test.wantOK)
			}
		})
	}
	root := "anchor \"com.apple/*\" all\n"
	if err := validateDarwinPolicy(root, "lo0", "000.old", "com.apple/100.guard", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinPolicy(root, "lo0", "000.old", "com.apple/100.guard", func(string) error { return errors.New("not empty") }); err == nil {
		t.Fatal("live earlier anchor accepted")
	}
}
