package machineguard

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Pure parsing of PF output for the macOS guard; platform-neutral so every OS's CI runs its tests.

const (
	appleHook = "apple" // anchor "com.apple/*" all
	ownHook   = "own"   // anchor "<guard anchor>" all, added by darwinInstallHook
)

// parseDarwinRoot finds the root rule that reaches the guard's anchor and returns the anchors that precede it
// and the quick rules that precede it. hook is "" when the root ruleset never reaches the guard.
func parseDarwinRoot(root, owned string) (hook string, beforeAnchors []string, err error) {
	var before []string
	for _, line := range strings.Split(root, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		switch {
		case line == "" || strings.HasPrefix(line, "scrub"):
		case line == `anchor "com.apple/*" all` || line == `anchor "`+owned+`" all`:
			if hook != "" {
				return "", nil, errors.New("duplicate Paperboat or Apple PF anchor in the root ruleset")
			}
			hook = appleHook
			if line != `anchor "com.apple/*" all` {
				hook = ownHook
			}
			for _, earlier := range before {
				name, isAnchor := darwinRootAnchorName(earlier)
				if isAnchor && !slices.Contains(strings.Fields(earlier), "quick") {
					beforeAnchors = append(beforeAnchors, name)
				} else if slices.Contains(strings.Fields(earlier), "quick") || strings.HasPrefix(earlier, "anchor ") {
					return "", nil, fmt.Errorf("PF root rule %q comes before Paperboat protection and would bypass it; move it after the `anchor \"com.apple/*\"` line of /etc/pf.conf and run `sudo pfctl -f /etc/pf.conf`", earlier)
				}
			}
		default:
			before = append(before, line)
		}
	}
	return hook, beforeAnchors, nil
}

// darwinRootAnchorName returns the anchor named by a root `anchor "name" ...` rule.
func darwinRootAnchorName(rule string) (string, bool) {
	rest, ok := strings.CutPrefix(rule, `anchor "`)
	name, _, found := strings.Cut(rest, `"`)
	name = strings.TrimSuffix(name, "/*")
	if !ok || !found || name == "" {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if !darwinAnchorName(part) {
			return "", false
		}
	}
	return name, true
}

func validateDarwinPolicy(root, interfaces, anchors, ownedAnchor string, inspectEarlier, inspectRoot func(string) error) (needHook bool, err error) {
	if !strings.HasPrefix(ownedAnchor, "com.apple/") {
		return false, errors.New("machine guard PF anchor must be an Apple child")
	}
	hook, beforeAnchors, err := parseDarwinRoot(root, ownedAnchor)
	if err != nil {
		return false, err
	}
	// pfctl -v prints '(skip)' when PFI_IFLAG_SKIP is active; no suffix means
	// filtering is enabled. Reject unknown output rather than infer protection.
	if strings.TrimSpace(interfaces) != "lo0" {
		return false, errors.New("PF is set to skip lo0 (`set skip on lo0` in the root ruleset); Paperboat protection needs PF to filter lo0")
	}
	for _, anchor := range beforeAnchors {
		if inspectRoot == nil {
			return false, errors.New("an earlier PF anchor could bypass protected machine names")
		}
		if err := inspectRoot(anchor); err != nil {
			return false, err
		}
	}
	if hook != appleHook {
		return hook == "", nil
	}
	own := strings.TrimPrefix(ownedAnchor, "com.apple/")
	for _, line := range strings.Split(anchors, "\n") {
		name := strings.TrimPrefix(strings.TrimSpace(line), "com.apple/")
		if name == "" || name == own {
			continue
		}
		if !darwinAnchorName(name) {
			return false, errors.New("unknown PF child anchor name")
		}
		if name < own {
			if inspectEarlier == nil {
				return false, errors.New("an earlier PF child anchor could bypass protected machine names")
			}
			if err := inspectEarlier("com.apple/" + name); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// darwinInsertHook puts the guard's anchor first among the root filter rules (after scrub rules, which PF
// requires first), so its quick rules decide before any rule of the user's.
func darwinInsertHook(root, anchor string) string {
	var out []string
	done := false
	for _, line := range strings.Split(strings.TrimRight(root, "\n"), "\n") {
		if !done && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "scrub") {
			out = append(out, `anchor "`+anchor+`" all`)
			done = true
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	if !done {
		out = append(out, `anchor "`+anchor+`" all`)
	}
	return strings.Join(out, "\n") + "\n"
}

func darwinRulesEmpty(rules string) error {
	if strings.TrimSpace(rules) != "" {
		return errors.New("an earlier PF child anchor contains rules that could bypass machine protection")
	}
	return nil
}

func darwinRulesNoQuick(rules string) error {
	for _, line := range strings.Split(rules, "\n") {
		if slices.Contains(strings.Fields(line), "quick") {
			return fmt.Errorf("PF anchor rule %q would bypass Paperboat protection; use the rule without `quick` or place its anchor after the Apple anchor", strings.TrimSpace(line))
		}
	}
	return nil
}

// PF retains empty named nodes after flushing a task-owned anchor. Only a fully
// inspected subtree whose rules pass check can precede the guard; unknown state fails closed.
func darwinEarlierAnchorOK(query func(...string) (string, error), anchor string, budget *int, depth int, check func(string) error) error {
	if *budget <= 0 || depth > 16 {
		return errors.New("PF anchor inspection limit exceeded")
	}
	*budget--
	rules, err := query("-a", anchor, "-sr")
	if err != nil {
		return err
	}
	if err = check(rules); err != nil {
		return err
	}
	children, err := query("-a", anchor, "-s", "Anchors")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(children, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, anchor+"/")
		if !darwinAnchorName(name) {
			return errors.New("unknown nested PF anchor name")
		}
		if err = darwinEarlierAnchorOK(query, anchor+"/"+name, budget, depth+1, check); err != nil {
			return err
		}
	}
	return nil
}
func darwinAnchorName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
