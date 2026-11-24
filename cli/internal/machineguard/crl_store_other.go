//go:build !windows

package machineguard

var platformPublishCRL func([]byte) error
