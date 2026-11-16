//go:build race

package connector

// raceEnabled skips timing measurements that race instrumentation distorts.
const raceEnabled = true
