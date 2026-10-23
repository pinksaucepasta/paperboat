package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// PLAN F19-15: the event catalog in server/docs/teams-business.md names exactly the namespaces the
// server writes. A namespace is the part of an event type before its first dot; event types are
// found where code names them (EventType: "ns.x", "ns." + verb, or a constant/variable named
// *Event / eventType).
var eventTypeSource = regexp.MustCompile(`(EventType|eventType|[A-Za-z]+Event)\s*(:=|=|:)\s*"([a-z_]+)\.`)

// notAuditEvents are strings that match the pattern but are not audit event types.
var notAuditEvents = map[string]string{
	"customer": "Stripe webhook event names (billing.TrialWillEndEvent)",
	"policy":   `the verb part of "personal." / "team." policy events`,
}

func TestEventCatalogDocumented(t *testing.T) {
	inCode := map[string]string{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "dbsqlc") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range eventTypeSource.FindAllStringSubmatch(string(raw), -1) {
			if _, skip := notAuditEvents[m[3]]; !skip {
				inCode[m[3]] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "teams-business.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(doc)
	start, end := strings.Index(section, "<!-- event-catalog:start -->"), strings.Index(section, "<!-- event-catalog:end -->")
	if start < 0 || end < start {
		t.Fatal("teams-business.md has no event-catalog section")
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|").FindAllStringSubmatch(section[start:end], -1) {
		documented[m[1]] = true
	}
	var missing, unused []string
	for ns, path := range inCode {
		if !documented[ns] {
			missing = append(missing, ns+" ("+path+")")
		}
	}
	for ns := range documented {
		if _, ok := inCode[ns]; !ok {
			unused = append(unused, ns)
		}
	}
	slices.Sort(missing)
	slices.Sort(unused)
	if len(missing) > 0 || len(unused) > 0 {
		t.Fatalf("event catalog differs from the code:\nwritten but not documented: %v\ndocumented but never written: %v", missing, unused)
	}
}
