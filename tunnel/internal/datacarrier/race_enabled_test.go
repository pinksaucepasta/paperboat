//go:build race

package datacarrier

// raceEnabled skips timing measurements that race instrumentation distorts.
const raceEnabled = true
