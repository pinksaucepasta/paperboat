package controlplane

import (
	"reflect"
	"testing"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A listed public preview's hostname is marked sign-in on its existing IP rule (lists kept) or on a new entry; other
// hosts are left as they were.
func TestMarkSignInKeepsRulesAndAddsMissing(t *testing.T) {
	rules := []wire.IngressIPRule{{Hostname: "a.test", Deny: []string{"203.0.113.0/24"}, FreeRequests: true}, {Hostname: "b.test", Allow: []string{"198.51.100.0/24"}}}
	listed := []wire.IngressDecision{{Binding: wire.IngressBinding{Hostname: "a.test"}}, {Binding: wire.IngressBinding{Hostname: "c.test"}}}
	got := markSignIn(rules, listed)
	want := []wire.IngressIPRule{{Hostname: "a.test", Deny: []string{"203.0.113.0/24"}, FreeRequests: true, SignIn: true}, {Hostname: "b.test", Allow: []string{"198.51.100.0/24"}}, {Hostname: "c.test", SignIn: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("markSignIn = %#v, want %#v", got, want)
	}
	if got := markSignIn(nil, nil); len(got) != 0 {
		t.Fatalf("no listed previews = %#v", got)
	}
}
