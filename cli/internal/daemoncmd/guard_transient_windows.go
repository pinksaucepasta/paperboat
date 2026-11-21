package daemoncmd

import "syscall"

// A named pipe whose guard went away fails writes with these instead of EPIPE.
var guardDisconnectErrors = []error{syscall.ERROR_BROKEN_PIPE, syscall.Errno(232) /* ERROR_NO_DATA */}
