package workspacedomain

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestApplyStatus(t *testing.T) {
	const host = "example.test"
	checked := dbsqlc.WorkspaceDomain{Hostname: host, DnsLastCheckedAt: sql.NullTime{Time: time.Now(), Valid: true}}
	records := func(apex bool, targets ...string) []Record {
		out := []Record{{Name: "_acme-challenge." + host, Type: "CNAME", Value: "c.challenge.test", Purpose: "verification", Required: true}}
		for _, p := range []string{"routing", "apex_routing"} {
			for _, target := range targets {
				name, typ := "*."+host, "A"
				if p == "apex_routing" {
					name = host
				}
				if strings.Contains(target, ":") {
					typ = "AAAA"
				}
				if target[0] > '9' {
					typ = "CNAME"
				}
				out = append(out, Record{Name: name, Type: typ, Value: target, Purpose: p, Required: p == "routing" || apex})
			}
		}
		return out
	}
	verif := "_acme-challenge." + host + " CNAME c.challenge.test"
	probe := "*." + host + " A "
	for _, tc := range []struct {
		name     string
		row      dbsqlc.WorkspaceDomain
		apex     bool
		targets  []string
		observed []string
		want     []string
		health   string
	}{
		{"healthy two targets", checked, false, []string{"192.0.2.1", "192.0.2.2"}, []string{verif, probe + "192.0.2.2", probe + "192.0.2.1"},
			[]string{RecordVerified, RecordVerified, RecordVerified, RecordNotChecked, RecordNotChecked}, HealthHealthy},
		{"one target missing", checked, false, []string{"192.0.2.1", "192.0.2.2"}, []string{verif, probe + "192.0.2.1"},
			[]string{RecordVerified, RecordVerified, RecordMissing, RecordNotChecked, RecordNotChecked}, HealthMissing},
		{"wrong address outranks missing", checked, false, []string{"192.0.2.1", "192.0.2.2"}, []string{verif, probe + "203.0.113.9"},
			[]string{RecordVerified, RecordWrongAddress, RecordWrongAddress, RecordNotChecked, RecordNotChecked}, HealthWrongAddress},
		{"none found", checked, false, []string{"192.0.2.1"}, []string{verif, probe + observedNone},
			[]string{RecordVerified, RecordMissing, RecordNotChecked}, HealthMissing},
		{"resolver failure is pending", checked, false, []string{"192.0.2.1"}, []string{verif, probe + observedUnavailable},
			[]string{RecordVerified, RecordPending, RecordNotChecked}, HealthChecking},
		{"not looked up is pending", checked, false, []string{"192.0.2.1"}, []string{verif},
			[]string{RecordVerified, RecordPending, RecordNotChecked}, HealthChecking},
		{"cname target", checked, false, []string{"d-1.edge.test"}, []string{verif, "*." + host + " CNAME D-1.Edge.Test."},
			[]string{RecordVerified, RecordVerified, RecordNotChecked}, HealthHealthy},
		{"apex used and missing", checked, true, []string{"192.0.2.1"}, []string{verif, probe + "192.0.2.1", host + " A " + observedNone},
			[]string{RecordVerified, RecordVerified, RecordMissing}, HealthMissing},
		{"apex used and verified", checked, true, []string{"192.0.2.1"}, []string{verif, probe + "192.0.2.1", host + " A 192.0.2.1"},
			[]string{RecordVerified, RecordVerified, RecordVerified}, HealthHealthy},
		{"dual stack: AAAA failure leaves A checked", checked, false, []string{"192.0.2.1", "2001:db8::1"}, []string{verif, probe + "203.0.113.9", "*." + host + " AAAA " + observedUnavailable},
			[]string{RecordVerified, RecordWrongAddress, RecordPending, RecordNotChecked, RecordNotChecked}, HealthWrongAddress},
		{"malformed and unknown lines ignored", checked, false, []string{"192.0.2.1"}, []string{verif, "*." + host + " BOGUS (none)", "garbage"},
			[]string{RecordVerified, RecordPending, RecordNotChecked}, HealthChecking},
		{"verification wrong", dbsqlc.WorkspaceDomain{Hostname: host, DnsLastCheckedAt: checked.DnsLastCheckedAt, DnsFailureCode: sql.NullString{String: "wrong_record", Valid: true}}, false, nil,
			[]string{"_acme-challenge." + host + " CNAME elsewhere.test"}, []string{RecordWrongAddress}, HealthWrongAddress},
		{"verification missing", dbsqlc.WorkspaceDomain{Hostname: host, DnsLastCheckedAt: checked.DnsLastCheckedAt, DnsFailureCode: sql.NullString{String: "dns_not_found", Valid: true}}, false, nil,
			nil, []string{RecordMissing}, HealthMissing},
		{"verification resolver failure", dbsqlc.WorkspaceDomain{Hostname: host, DnsLastCheckedAt: checked.DnsLastCheckedAt, DnsFailureCode: sql.NullString{String: "dns_unavailable", Valid: true}}, false, nil,
			nil, []string{RecordPending}, HealthChecking},
		{"never checked", dbsqlc.WorkspaceDomain{Hostname: host}, false, nil, nil, []string{RecordPending}, HealthChecking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := records(tc.apex, tc.targets...)
			health := applyStatus(tc.row, recs, tc.observed)
			var got []string
			for _, r := range recs {
				got = append(got, r.Status)
				if r.ObservedValues == nil {
					t.Fatalf("%s observed_values is nil", r.Name)
				}
			}
			if !reflect.DeepEqual(got, tc.want) || health != tc.health {
				t.Fatalf("statuses %v health %s, want %v %s", got, health, tc.want, tc.health)
			}
		})
	}
}

func TestObserveRoutingSkipsUnusedApexAndBoundsValues(t *testing.T) {
	const host = "example.test"
	for _, apex := range []bool{false, true} {
		var asked []string
		lookup := func(_ context.Context, name string, qtype uint16) ([]string, error) {
			asked = append(asked, name+" "+dns.TypeToString[qtype])
			if name == host {
				return nil, errors.New("timeout")
			}
			return make([]string, 40), nil
		}
		recs := []Record{
			{Name: "_acme-challenge." + host, Type: "CNAME", Purpose: "verification", Required: true},
			{Name: "*." + host, Type: "A", Value: "192.0.2.1", Purpose: "routing", Required: true},
			{Name: "*." + host, Type: "A", Value: "192.0.2.2", Purpose: "routing", Required: true},
			{Name: host, Type: "A", Value: "192.0.2.1", Purpose: "apex_routing", Required: apex},
		}
		got := observeRouting(context.Background(), lookup, recs)
		if len(asked) == 0 || !strings.HasPrefix(asked[0], "_pb-probe-") || !strings.HasSuffix(asked[0], "."+host+" A") {
			t.Fatalf("wildcard probe = %v", asked)
		}
		asked[0] = "probe"
		want := []string{"probe"}
		wantLines := maxObservedValues
		if apex {
			want, wantLines = append(want, host+" A"), wantLines+1
		}
		if !reflect.DeepEqual(asked, want) || len(got) != wantLines {
			t.Fatalf("apex=%v asked %v, %d lines, want %v %d", apex, asked, len(got), want, wantLines)
		}
		if apex && got[len(got)-1] != host+" A "+observedUnavailable {
			t.Fatalf("apex resolver failure stored as %q", got[len(got)-1])
		}
	}
}

func TestApplyStatusANAME(t *testing.T) {
	const host = "example.test"
	row := dbsqlc.WorkspaceDomain{Hostname: host, DnsLastCheckedAt: sql.NullTime{Time: time.Now(), Valid: true}}
	for _, tc := range []struct {
		observed []string
		want     string
	}{
		{[]string{host + " A 192.0.2.1", "edge.test A 192.0.2.1"}, RecordVerified},
		{[]string{host + " A 203.0.113.9", "edge.test A 192.0.2.1"}, RecordWrongAddress},
		{[]string{host + " A (none)", "edge.test A 192.0.2.1"}, RecordMissing},
		{[]string{host + " A 192.0.2.1", "edge.test A (unavailable)"}, RecordPending},
	} {
		recs := []Record{{Name: host, Type: "ANAME", Value: "edge.test", Purpose: "apex_routing", Required: true}}
		applyStatus(row, recs, tc.observed)
		if recs[0].Status != tc.want {
			t.Fatalf("%v: status %s, want %s", tc.observed, recs[0].Status, tc.want)
		}
	}
}
