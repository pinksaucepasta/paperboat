package main

import "time"

// daemonSnapshotLookupTimeout bounds the extra daemon read a failed command makes to explain itself.
const daemonSnapshotLookupTimeout = 2 * time.Second
