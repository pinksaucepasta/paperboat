//go:build !js || !wasm

package main

// The browser terminal client is built for js/wasm. This native entry point
// keeps its certificate, envelope, and framing rules testable with Go tests.
func main() {}
