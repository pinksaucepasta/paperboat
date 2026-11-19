package testleak

import (
	"testing"

	"go.uber.org/goleak"
)

// stdlibDNS: the stdlib DNS singleflight worker detaches its query onto a never-expiring context
// (withUnexpiredValuesPreserved, src/net/lookup.go), so under host load a slow stub-resolver answer can still be in
// flight when a test's leak check runs (the full cli -race suite failed on two such workers with passing subtests).
// Ignoring exactly that top frame leaves product goroutines fully guarded.
var stdlibDNS = goleak.IgnoreTopFunction("net.(*Resolver).lookupIPAddr.func2")

// Check fails t at cleanup when the test leaves a goroutine that was not running when Check was called. Every
// goroutine leak check in the module goes through it, so the one known stdlib exception is written once.
func Check(t testing.TB, extra ...goleak.Option) {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, append([]goleak.Option{baseline, stdlibDNS}, extra...)...) })
}
