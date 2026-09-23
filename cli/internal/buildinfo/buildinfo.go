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

// DefaultServerURL is the first-party Paperboat control plane. Private builds
// may replace it with -ldflags, but official binaries work without setup.
var DefaultServerURL = "https://api.pprbt.dev"

// DefaultReleaseURL is retained for source compatibility. Release builds set
// it to DefaultServerURL; the control plane is the sole update origin.
var DefaultReleaseURL = DefaultServerURL
