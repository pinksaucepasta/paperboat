//go:build !windows

package pty

import "errors"

// FlushConsoleInput exists so the hidden pb daemon __flush-console-input command builds everywhere; only Windows uses it.
func FlushConsoleInput() error { return errors.New("console input flush exists only on Windows") }
