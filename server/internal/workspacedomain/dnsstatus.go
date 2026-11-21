package workspacedomain

import (
	"context"
	"crypto/rand"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// Record status: what DNS answers for a record's name compared with what the user was asked to add.
const (
	RecordVerified     = "verified"
	RecordWrongAddress = "wrong_address"
	RecordMissing      = "missing"
	RecordPending      = "pending"     // not checked yet, or the resolver failed (proves nothing)
	RecordNotChecked   = "not_checked" // optional record nothing uses (the apex without a root-domain tunnel/preview)
)

// DNS health of a whole domain over its required records: wrong_address outranks missing outranks checking.
const (
	HealthHealthy      = "healthy"
	HealthWrongAddress = "wrong_address"
	HealthMissing      = "missing"
	HealthChecking     = "checking"
)

const (
	maxObservedValues = 16
	lookupTimeout     = 10 * time.Second
	// Stored observation lines are "<record name> <TYPE> <value>"; these values mark a lookup without answers.
	observedNone        = "(none)"
	observedUnavailable = "(unavailable)"
)

// queryType is what is looked up for a record type: an ANAME (apex alias) answers with addresses.
func queryType(t string) string {
	if t == "ANAME" {
		return "A"
	}
	return t
}

// observeRouting looks up the required routing records' names once per type, the wildcard through a fresh
// random label (so no explicit record can stand in for it), and an ANAME's target too, whose addresses
// are what the alias must answer. It never touches ownership.
func observeRouting(ctx context.Context, lookup DNSLookup, records []Record) []string {
	seen := map[string]bool{}
	out := []string{}
	observe := func(name, typ, query string) {
		key := name + " " + typ
		qtype, ok := dns.StringToType[typ]
		if seen[key] || !ok {
			return
		}
		seen[key] = true
		lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		values, err := lookup(lctx, query, qtype)
		cancel()
		switch {
		case err != nil:
			out = append(out, key+" "+observedUnavailable)
		case len(values) == 0:
			out = append(out, key+" "+observedNone)
		default:
			for _, v := range values[:min(len(values), maxObservedValues)] {
				out = append(out, key+" "+v)
			}
		}
	}
	for _, r := range records {
		if r.Purpose == "verification" || !r.Required {
			continue
		}
		query := r.Name
		if strings.HasPrefix(r.Name, "*.") {
			query = "_pb-probe-" + strings.ToLower(rand.Text()[:12]) + r.Name[1:]
		}
		observe(r.Name, queryType(r.Type), query)
		if r.Type == "ANAME" {
			observe(r.Value, "A", r.Value)
		}
	}
	return out
}

type observation struct {
	unavailable bool
	values      []string
}

// parseObserved keys observations by "<name> <TYPE>"; lines of other shapes or types are ignored.
func parseObserved(lines []string) map[string]*observation {
	out := map[string]*observation{}
	for _, line := range lines {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || (parts[1] != "A" && parts[1] != "AAAA" && parts[1] != "CNAME") {
			continue
		}
		key := parts[0] + " " + parts[1]
		o := out[key]
		if o == nil {
			o = &observation{}
			out[key] = o
		}
		switch parts[2] {
		case observedUnavailable:
			o.unavailable = true
		case observedNone:
		default:
			o.values = append(o.values, normalizeValue(parts[2]))
		}
	}
	return out
}

func normalizeValue(v string) string {
	if a, err := netip.ParseAddr(v); err == nil {
		return a.Unmap().String()
	}
	return strings.ToLower(strings.TrimSuffix(v, "."))
}

// applyStatus sets each record's status and observed values and returns the domain's DNS health.
func applyStatus(row dbsqlc.WorkspaceDomain, records []Record, observedLines []string) string {
	observed := parseObserved(observedLines)
	// wants are the values each record accepts: its value, or for an ANAME its target's addresses.
	wants := make([][]string, len(records))
	expected := map[string]map[string]bool{} // per name: every value any required record there accepts
	pending := make([]bool, len(records))
	for i, r := range records {
		if r.Purpose == "verification" || !r.Required {
			continue
		}
		wants[i] = []string{normalizeValue(r.Value)}
		if r.Type == "ANAME" {
			target := observed[r.Value+" A"]
			pending[i] = target == nil || target.unavailable
			if target != nil {
				wants[i] = target.values
			}
		}
		if expected[r.Name] == nil {
			expected[r.Name] = map[string]bool{}
		}
		for _, w := range wants[i] {
			expected[r.Name][w] = true
		}
	}
	for i := range records {
		r := &records[i]
		r.ObservedValues = []string{}
		o := observed[r.Name+" "+queryType(r.Type)]
		if o != nil {
			r.ObservedValues = append(r.ObservedValues, o.values...)
		}
		switch {
		case r.Purpose == "verification":
			r.Status = verificationStatus(row)
		case !r.Required:
			r.Status = RecordNotChecked
		case o == nil || o.unavailable || pending[i]:
			r.Status = RecordPending
		default:
			r.Status = routingStatus(o.values, expected[r.Name], wants[i])
		}
	}
	health := HealthHealthy
	rank := map[string]int{HealthHealthy: 0, HealthChecking: 1, HealthMissing: 2, HealthWrongAddress: 3}
	for _, r := range records {
		h := map[string]string{RecordVerified: HealthHealthy, RecordPending: HealthChecking, RecordMissing: HealthMissing, RecordWrongAddress: HealthWrongAddress}[r.Status]
		if r.Required && rank[h] > rank[health] {
			health = h
		}
	}
	return health
}

func routingStatus(values []string, expected map[string]bool, wants []string) string {
	found := false
	for _, v := range values {
		if !expected[v] {
			return RecordWrongAddress
		}
		found = found || slices.Contains(wants, v)
	}
	if !found {
		return RecordMissing
	}
	return RecordVerified
}

func verificationStatus(row dbsqlc.WorkspaceDomain) string {
	if !row.DnsLastCheckedAt.Valid {
		return RecordPending
	}
	switch row.DnsFailureCode.String {
	case "dns_not_found":
		return RecordMissing
	case "wrong_record":
		return RecordWrongAddress
	case "":
		return RecordVerified
	default:
		return RecordPending
	}
}

func apexInUse(uses []Use, hostname string) bool {
	for _, u := range uses {
		if u.Hostname == hostname {
			return true
		}
	}
	return false
}
