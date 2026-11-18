// Package tufroot is the one trust anchor of Paperboat's signed release metadata (TUF root). pb verifies its own
// updates with it and pbh the self-host bundle; release tooling signs against the same file.
package tufroot

import _ "embed"

// TrustedRoot is the embedded TUF root metadata (root.json).
//
//go:embed trusted-root.json
var TrustedRoot []byte
