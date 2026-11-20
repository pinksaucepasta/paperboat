package managedssh

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"slices"
	"time"

	"golang.org/x/crypto/ssh"
)

var ErrSSHAuthentication = errors.New("managed SSH authentication is unavailable")

type sshAuthenticationFailure struct{ cause error }

func (e *sshAuthenticationFailure) Error() string { return ErrSSHAuthentication.Error() }
func (e *sshAuthenticationFailure) Unwrap() []error {
	if e == nil || e.cause == nil {
		return []error{ErrSSHAuthentication}
	}
	return []error{ErrSSHAuthentication, e.cause}
}

func sshAuthenticationError(cause error) error {
	return &sshAuthenticationFailure{cause: cause}
}

// ProbeSSHAuthentication completes only the SSH transport and user-auth phases.
// It opens no session, shell, command, subsystem, or forwarding channel.
func ProbeSSHAuthentication(ctx context.Context, stream io.ReadWriteCloser, address, user string, signer ssh.Signer, authorizedHostKeys []string) error {
	if ctx == nil || stream == nil || address == "" || user == "" || signer == nil || len(authorizedHostKeys) == 0 {
		return ErrSSHAuthentication
	}
	hostKeys, err := ParseHostPublicKeys(authorizedHostKeys)
	if err != nil {
		return errors.Join(ErrSSHAuthentication, err)
	}
	allowed := make([][32]byte, len(hostKeys))
	for index := range hostKeys {
		allowed[index] = hostKeys[index].Fingerprint
	}
	config := &ssh.ClientConfig{
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: openSSHHostKeyAlgorithms(hostKeys),
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if slices.Contains(allowed, sha256.Sum256(key.Marshal())) {
				return nil
			}
			return ErrKnownHosts
		},
	}
	results := make(chan sshProbeResult, 1)
	go func() {
		connection, _, _, handshakeErr := ssh.NewClientConn(sshProbeConn{ReadWriteCloser: stream}, address, config)
		results <- sshProbeResult{connection: connection, err: handshakeErr}
	}()
	return awaitSSHProbe(ctx, stream, results)
}

type sshProbeResult struct {
	connection ssh.Conn
	err        error
}

// awaitSSHProbe reports the handshake's outcome. A handshake that already finished wins over a done ctx: both can be
// ready together and a random pick reported a successful authentication as an SSH authentication failure.
func awaitSSHProbe(ctx context.Context, stream io.Closer, results <-chan sshProbeResult) error {
	var outcome sshProbeResult
	select {
	case outcome = <-results:
	case <-ctx.Done():
		select {
		case outcome = <-results:
		default:
			return sshAuthenticationError(errors.Join(managedSSHContextError(ctx), stream.Close()))
		}
	}
	if outcome.connection != nil {
		outcome.err = errors.Join(outcome.err, outcome.connection.Close())
	}
	if outcome.err != nil {
		return sshAuthenticationError(outcome.err)
	}
	return nil
}

type sshProbeConn struct{ io.ReadWriteCloser }

func (sshProbeConn) LocalAddr() net.Addr              { return sshProbeAddress("local") }
func (sshProbeConn) RemoteAddr() net.Addr             { return sshProbeAddress("remote") }
func (sshProbeConn) SetDeadline(time.Time) error      { return nil }
func (sshProbeConn) SetReadDeadline(time.Time) error  { return nil }
func (sshProbeConn) SetWriteDeadline(time.Time) error { return nil }

type sshProbeAddress string

func (sshProbeAddress) Network() string  { return "paperboat" }
func (a sshProbeAddress) String() string { return string(a) }
