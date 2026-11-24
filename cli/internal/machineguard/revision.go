//go:build linux || darwin || windows

package machineguard

// Revision orders machine guards across development builds, whose versions do not order: the guard is
// one per machine and shared by every OS user's install, so an install replaces it only with a higher
// revision (FIX-MULTIINST 2a602424f fixed the guard without adding a feature and never reached machines).
// Bump it with any change to this package's code; TestGuardRevisionMatchesItsSource fails until you do.
const Revision = 20

// RevisionSourceSHA256 is the hash of this package's code (non-test .go files except this one) that
// Revision describes. Update it together with Revision.
const RevisionSourceSHA256 = "7460020d364057f49072e2d2f7a314ccd7f3b9eb1673cb5316af569347bfcde1"
