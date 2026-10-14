package publicationdns

import "testing"

func TestTargetAndRecordType(t *testing.T) {
	for _, c := range []struct{ selected, host, provider, wantTarget, wantType string }{
		{"140.245.11.41", "www.example.com", "generic", "140.245.11.41", "A"},
		{"[2001:db8::1]", "example.com", "route53", "2001:db8::1", "AAAA"},
		{"edge.example.net", "example.com", "route53", "edge.example.net", "ALIAS"},
		{"", "www.example.com", "generic", "stable.example.net", "CNAME"},
		{"", "example.com", "generic", "stable.example.net", "ANAME"},
	} {
		target, err := Target("https://stable.example.net", c.selected)
		if err != nil || target != c.wantTarget || DomainRecordType(target, c.host, c.provider) != c.wantType {
			t.Fatalf("case %+v target %q err %v", c, target, err)
		}
	}
}
