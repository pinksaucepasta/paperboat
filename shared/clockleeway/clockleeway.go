// Package clockleeway is the one allowance for a machine whose clock is off when a validity window
// (certificate, grant) is checked, before serverclock has learned the offset from a server response.
// Like Teleport's certificate clock-skew allowance; a clock further off fails loudly (`pb doctor`,
// clock_skew). It has no imports so the browser (TinyGo) build can use it.
package clockleeway

import "time"

const Leeway = 15 * time.Minute

// Valid reports whether now lies in [notBefore, notAfter) widened by Leeway on both sides.
func Valid(now, notBefore, notAfter time.Time) bool {
	return !now.Add(Leeway).Before(notBefore) && now.Add(-Leeway).Before(notAfter)
}
