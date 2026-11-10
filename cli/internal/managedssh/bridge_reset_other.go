//go:build !windows

package managedssh

// closeResets: on Unix a connection reset is a failure, a normal end reads as EOF.
var closeResets []error
