//go:build linux || darwin || windows

package machineguard

// Revision orders machine guards across development builds, whose versions do not order: the guard is
// one per machine and shared by every OS user's install, so an install replaces it only with a higher
// revision (FIX-MULTIINST 2a602424f fixed the guard without adding a feature and never reached machines).
// Bump it with any change to this package's code; TestGuardRevisionMatchesItsSource fails until you do.
const Revision = 21

// RevisionSourceSHA256 is the hash of this package's code (non-test .go files except this one) that
// Revision describes. Update it together with Revision.
const RevisionSourceSHA256 = "97bf228228837d5bce4f261badafcade8796d9d35f431801a50f16a54aa33f85"
