package managedssh

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

// A host that refuses an SSH stream writes one refusal line in place of sshd's banner, so pb's ProxyCommand can
// say why instead of OpenSSH's "Connection closed" (F5-09). sshd's banner starts with "SSH-", never with this.
const refusalPrefix = "PAPERBOAT-REFUSED "

// RefusedError is a stream the host refused, with the reason it sent.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string {
	switch e.Reason {
	case RefusedBusy:
		return "the machine is at its limit of concurrent SSH sessions; close one there or try again shortly"
	case RefusedNoAccess:
		return "you do not have SSH access to this machine; ask a team admin to grant it"
	case RefusedNotSetUp:
		return "SSH is not set up on this machine yet; run `pb ssh doctor <machine>` for the next step"
	default:
		return "the machine refused the SSH session (" + e.Reason + ")"
	}
}

// writeRefusal is bounded: a client that never reads must not hold the host. A stream without deadlines gets none.
func writeRefusal(stream io.Writer, reason string) {
	if deadline, ok := stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = deadline.SetWriteDeadline(time.Now().Add(time.Second))
	}
	_, _ = fmt.Fprintf(stream, "%s%s\r\n", refusalPrefix, reason)
}

// ReadRefusal returns a RefusedError when the stream starts with a host's refusal line, and otherwise leaves every
// byte in r for the SSH client.
func ReadRefusal(r *bufio.Reader) error {
	if first, err := r.Peek(1); err != nil || first[0] != refusalPrefix[0] {
		return nil
	}
	if head, err := r.Peek(len(refusalPrefix)); err != nil || !bytes.Equal(head, []byte(refusalPrefix)) {
		return nil
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return nil
	}
	return &RefusedError{Reason: strings.TrimSpace(strings.TrimPrefix(line, refusalPrefix))}
}
