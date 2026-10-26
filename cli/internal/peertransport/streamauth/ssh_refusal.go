package streamauth

import (
	"fmt"
	"io"
	"time"
)

// SSHRefusalPrefix starts the one line a host writes in place of sshd's banner when it refuses an SSH stream,
// so pb's ProxyCommand can say why (F5-09); sshd's banner starts with "SSH-", never with this.
const SSHRefusalPrefix = "PAPERBOAT-REFUSED "

// WriteSSHRefusal writes the refusal line with reason, bounded: a client that never reads must not hold the
// host. A stream without deadlines gets none.
func WriteSSHRefusal(stream io.Writer, reason string) {
	if deadline, ok := stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = deadline.SetWriteDeadline(time.Now().Add(time.Second))
	}
	_, _ = fmt.Fprintf(stream, "%s%s\r\n", SSHRefusalPrefix, reason)
}
