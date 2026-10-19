// Package buildinfo holds the identity of the running server build. The release and local
// build pipelines set both values with linker flags.
package buildinfo

// Version is the release tag the image was built for.
var Version = "development"

// Commit is the 40-hex source commit of the exact tree that was built; "unknown" when not set.
var Commit = "unknown"
