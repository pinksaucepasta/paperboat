package managedssh

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/streamauth"
)

// A host that refuses an SSH stream writes one refusal line in place of sshd's banner (streamauth.WriteSSHRefusal).
const refusalPrefix = streamauth.SSHRefusalPrefix

// ErrSSHServerClosed is a stream that ended before any byte arrived: no banner and no refusal line. Nothing reached
// the machine's sshd-side answer, so the cause may be inside Paperboat's own transport, not sshd.
var ErrSSHServerClosed = errors.New("Paperboat's connection to the machine closed before its SSH server answered (no bytes came back, so the SSH server may never have seen the connection). Run `pb doctor` and check the Paperboat service on both machines, then retry")

// RefusedError is a stream the host refused, with the reason it sent.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string {
	switch e.Reason {
	case RefusedNoAccess:
		return "you do not have SSH access to this machine; ask a team admin to grant it"
	case RefusedNotSetUp:
		return "SSH is not set up on this machine yet; run `pb ssh doctor <machine>` for the next step"
	default:
		if user, ok := strings.CutPrefix(e.Reason, RefusedLoginNotAllowed+":"); ok {
			return user + " isn't allowed to log in over SSH on this machine: on the Mac open System Settings → General → Sharing → Remote Login and allow this user, then retry"
		}
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

// ReadServerStart is ReadRefusal for pb's own SSH client: a stream that ends before its first byte is also named
// (ErrSSHServerClosed), where the client would only report a bare handshake EOF.
func ReadServerStart(r *bufio.Reader) error {
	if _, err := r.Peek(1); errors.Is(err, io.EOF) {
		return ErrSSHServerClosed
	}
	return ReadRefusal(r)
}

// startedStream reads through the reader that already took the first bytes of the stream.
type startedStream struct {
	io.ReadWriteCloser
	reader *bufio.Reader
}

func (s startedStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
