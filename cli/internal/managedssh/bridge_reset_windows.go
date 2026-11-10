//go:build windows

package managedssh

import "syscall"

// closeResets are the errors a bridge's normal end can end with on Windows: Windows OpenSSH closes its loopback connection with an RST, which Winsock
// reports as WSAECONNRESET (10054; syscall.ECONNRESET is a different, invented number there), where a Unix sshd's FIN reads as EOF. Without it a session
// that moved data both ways and ended normally was audited outcome `failed`.
var closeResets = []error{syscall.Errno(10054)}
