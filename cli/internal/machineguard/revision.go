//go:build linux || darwin || windows

package machineguard

// Revision orders machine guards across development builds, whose versions do not order: the guard is
// one per machine and shared by every OS user's install, so an install replaces it only with a higher
// revision (FIX-MULTIINST 2a602424f fixed the guard without adding a feature and never reached machines).
// Bump it with any change to this package's code; TestGuardRevisionMatchesItsSource fails until you do.
const Revision = 28

// RevisionSourceSHA256 is the hash of this package's code (non-test .go files except this one) that
// Revision describes. Update it together with Revision.
const RevisionSourceSHA256 = "7f7cf9060f3509cd1716c92bab4cf8e1568a1ba5d407c122f06041a366bc6d37"
