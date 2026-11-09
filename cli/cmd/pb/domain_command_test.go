package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestWorkspaceDomainOutputListsRecordsStatusAndUses(t *testing.T) {
	d := api.WorkspaceDomain{ID: "wsd_0123456789ab", Hostname: "dev.example.com", State: "pending_dns",
		DNSFailure: &api.WorkspaceDomainFailure{Code: "dns_not_found", Message: "No CNAME at _acme-challenge.dev.example.com yet."},
		Records: []api.WorkspaceDomainRecord{
			{Name: "_acme-challenge.dev.example.com", Type: "CNAME", Value: "pb-1.challenges.example.test", Purpose: "verification", Required: true},
			{Name: "*.dev.example.com", Type: "CNAME", Value: "d-1.tunnels.example.test", Purpose: "routing", Required: true},
			{Name: "dev.example.com", Type: "ANAME", Value: "d-1.tunnels.example.test", Purpose: "apex_routing"},
		},
		UsedBy: []api.WorkspaceDomainUse{{Kind: "tunnel", ID: "tun_1", Name: "web", Hostname: "app.dev.example.com"}}}
	command := domainShowCommandV1()
	var out bytes.Buffer
	command.SetOut(&out)
	if err := writeWorkspaceDomain(command, d, "note"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"waiting for DNS", "No CNAME at", "_acme-challenge.dev.example.com\tCNAME\tpb-1.challenges.example.test\t(required: proves", "*.dev.example.com\tCNAME", "optional: only to serve dev.example.com", "https://app.dev.example.com\ttunnel web", "note"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
	if err := workspaceDomainInUse(d); err == nil || !strings.Contains(err.Error(), "app.dev.example.com (tunnel web)") {
		t.Fatalf("in-use error = %v", err)
	}
}

func TestDomainCommandTree(t *testing.T) {
	var names []string
	for _, c := range domainCobraCommand().Commands() {
		names = append(names, c.Name())
	}
	if strings.Join(names, ",") != "add,list,remove,show,verify" {
		t.Fatalf("pb domain subcommands = %v", names)
	}
}
