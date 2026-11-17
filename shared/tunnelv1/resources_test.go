package tunnelv1

import (
	"encoding/json"
	"testing"
)

// The server's output defines these: the domain's certificate strategy and observed records are always present
// (the cli used to omit them when empty), a log entry omits an empty tunnel id.
func TestServerOutputDefinesDomainAndLog(t *testing.T) {
	raw, err := json.Marshal(Domain{ID: "d", State: "pending", DNS: DomainDNS{Target: "t"}, Certificate: DomainCertificate{State: "none"}})
	want := `{"schema":"","kind":"","id":"d","account_id":"","tunnel_id":"","route_id":"","hostname":"","match_type":"","certificate_strategy":"","state":"pending","dns":{"target":"t","observed_records":null},"certificate":{"state":"none"},"generation":0,"etag":""}`
	if err != nil || string(raw) != want {
		t.Fatalf("domain = %s, %v", raw, err)
	}
	raw, err = json.Marshal(LogEntry{ID: "l"})
	if err != nil || string(raw) == "" || json.Valid(raw) == false {
		t.Fatalf("log = %s, %v", raw, err)
	}
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if _, present := back["tunnel_id"]; present {
		t.Fatal("empty tunnel_id must be omitted")
	}
}
