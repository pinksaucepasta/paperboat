// Package buildinfo holds build-time metadata that release builds override via
// -ldflags. Keeping these as vars (not consts) lets the linker stamp them.
package buildinfo

// Version is the CLI version. Replaced by release builds.
var Version = "dev"

// Distribution defaults to custom. Only official release builds stamp official;
// it selects update policy and does not replace verification of downloaded bytes.
var Distribution = "custom"

// Official release pipelines inject these defaults. Ordinary source builds have
// no telemetry destination. These public values are not proof of binary origin.
var DefaultSentryDSN = ""
var DefaultSentryRelease = ""

// WindowsPublisher is the required Authenticode signer subject fragment for
// Windows release components. Release builds replace it with the legal
// publisher name encoded in the signing certificate.
var WindowsPublisher = "Paperboat"

// Commit is the source revision. Replaced by release builds.
var Commit = "unknown"

// ProtocolVersion is the control-plane contract understood by this binary.
var ProtocolVersion = "1"

// DefaultServerURL is the control plane this build talks to unless --server or
// the configuration says otherwise. Every build sets it with -ldflags (the
// Makefile and release tooling do); source has no domain of its own.
var DefaultServerURL = ""

// DefaultReleaseURL is the independent official release host, set by release
// builds. Source builds have no update origin.
var DefaultReleaseURL = ""
