package managedssh

import (
	"bufio"
	"bytes"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

// A host that refuses an SSH stream writes one refusal line in place of sshd's banner (streamauth.WriteSSHRefusal).
const refusalPrefix = streamauth.SSHRefusalPrefix

// ErrSSHServerClosed is a connection the machine's sshd ended before its banner: it refused it (MaxStartups,
// a ban list) or is not serving. Paperboat itself sets no session limit; the machine's sshd decides.
var ErrSSHServerClosed = errors.New("the machine's SSH server closed the connection before answering; it may be refusing new connections (its sshd MaxStartups or MaxSessions limit) or not running. Retry shortly, or check sshd on that machine")

// RefusedError is a stream the host refused, with the reason it sent.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string {
	switch e.Reason {
	case RefusedNoAccess:
		return "you do not have SSH access to this machine; ask a team admin to grant it"
	case RefusedNotSetUp:
		return "SSH is not set up on this machine yet; run `pb ssh doctor <machine>` for the next step"
	default:
		return "the machine refused the SSH session (" + e.Reason + ")"
	}
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
