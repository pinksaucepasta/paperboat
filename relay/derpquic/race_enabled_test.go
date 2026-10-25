//go:build race

package derpquic

// raceEnabled skips timing budgets that the race detector slows several times over.
const raceEnabled = true
