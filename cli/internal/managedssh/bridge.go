package managedssh

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

var (
	ErrSSHTargetInvalid     = errors.New("managed SSH target is invalid")
	ErrSSHTargetUnavailable = errors.New("managed SSH loopback target is unavailable")
)

type LoopbackTarget struct {
	Host string
	Port uint16
}

type SSHReadiness struct {
	IPv4   bool
	IPv6   bool
	Target LoopbackTarget
}

type BridgeResult struct {
	ToSSHD   int64
	FromSSHD int64
}

func NewLoopbackTarget(host string, port uint16) (LoopbackTarget, error) {
	if port == 0 || host != "127.0.0.1" && host != "::1" {
		return LoopbackTarget{}, ErrSSHTargetInvalid
	}
	return LoopbackTarget{Host: host, Port: port}, nil
}

func ProbeLoopbackSSH(ctx context.Context, port uint16, timeout time.Duration) (SSHReadiness, error) {
	if ctx == nil || port == 0 || timeout <= 0 || timeout > 30*time.Second {
		return SSHReadiness{}, ErrSSHTargetInvalid
	}
	type probe struct {
		host string
		err  error
	}
	results := make(chan probe, 2)
	for _, host := range []string{"127.0.0.1", "::1"} {
		go func(host string) {
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
			if err == nil {
				err = connection.Close()
			}
			results <- probe{host: host, err: err}
		}(host)
	}
	var readiness SSHReadiness
	probeFailures := make([]error, 0, 2)
	for range 2 {
		result := <-results
		if result.err != nil {
			probeFailures = append(probeFailures, result.err)
			continue
		}
		if result.host == "127.0.0.1" {
			readiness.IPv4 = true
		} else {
			readiness.IPv6 = true
		}
	}
	selected := ""
	if readiness.IPv4 {
		selected = "127.0.0.1"
	} else if readiness.IPv6 {
		selected = "::1"
	}
	if selected == "" {
		if ctx.Err() != nil {
			return readiness, managedSSHContextError(ctx)
		}
		return readiness, managedSSHFailure("target_connect", ErrSSHTargetUnavailable, errors.Join(probeFailures...))
	}
	readiness.Target = LoopbackTarget{Host: selected, Port: port}
	return readiness, nil
}

func BridgeSSH(ctx context.Context, stream io.ReadWriteCloser, target LoopbackTarget, dialTimeout time.Duration) (BridgeResult, error) {
	if ctx == nil || stream == nil || dialTimeout <= 0 || dialTimeout > 30*time.Second {
		return BridgeResult{}, ErrSSHTargetInvalid
	}
	if _, err := NewLoopbackTarget(target.Host, target.Port); err != nil {
		return BridgeResult{}, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	sshd, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))))
	cancelDial()
	if err != nil {
		if ctx.Err() != nil {
			return BridgeResult{}, managedSSHContextError(ctx)
		}
		return BridgeResult{}, managedSSHFailure("target_connect", ErrSSHTargetUnavailable, err)
	}
	var closeOnce sync.Once
	var closeErr error
	finish := func() error {
		closeOnce.Do(func() { closeErr = errors.Join(sshd.Close(), stream.Close()) })
		return closeErr
	}
	stop := context.AfterFunc(ctx, func() {
		_ = finish()
	})
	defer stop()
	defer finish()
	type copyResult struct {
		direction string
		bytes     int64
		err       error
	}
	results := make(chan copyResult, 2)
	go func() {
		count, copyErr := io.Copy(sshd, stream)
		copyErr = normalizeBridgeError(copyErr)
		if tcp, ok := sshd.(*net.TCPConn); ok {
			copyErr = errors.Join(copyErr, normalizeBridgeError(tcp.CloseWrite()))
		}
		if copyErr != nil {
			_ = finish()
		}
		results <- copyResult{direction: "to_sshd", bytes: count, err: copyErr}
	}()
	go func() {
		count, copyErr := io.Copy(stream, sshd)
		copyErr = normalizeBridgeError(copyErr)
		if half, ok := stream.(interface{ CloseWrite() error }); ok {
			copyErr = errors.Join(copyErr, normalizeBridgeError(half.CloseWrite()))
		}
		// Target EOF is terminal for an SSH connection. Some OpenSSH clients
		// keep ProxyCommand input open until the proxy exits, so waiting for the
		// opposite copier here deadlocks after a remote command has completed.
		// Drain target output, publish its FIN, then close both read sides to
		// unblock the client-to-target copier.
		_ = finish()
		results <- copyResult{direction: "from_sshd", bytes: count, err: copyErr}
	}()
	var result BridgeResult
	var resultErr error
	for range 2 {
		copied := <-results
		if copied.direction == "to_sshd" {
			result.ToSSHD = copied.bytes
		} else {
			result.FromSSHD = copied.bytes
		}
		resultErr = errors.Join(resultErr, copied.err)
	}
	cleanupErr := normalizeBridgeError(finish())
	if ctx.Err() != nil {
		if resultErr == nil && cleanupErr == nil || managedSSHLeavesMatch(errors.Join(resultErr, cleanupErr), io.EOF, io.ErrClosedPipe, net.ErrClosed, context.Canceled) {
			return result, managedSSHContextError(ctx)
		}
		return result, managedSSHBoundary("command", errors.Join(ctx.Err(), context.Cause(ctx), resultErr, cleanupErr))
	}
	return result, managedSSHBoundary("command", errors.Join(resultErr, cleanupErr))
}

func normalizeBridgeError(err error) error {
	if err == nil || managedSSHLeavesMatch(err, io.EOF, io.ErrClosedPipe, net.ErrClosed) {
		return nil
	}
	return err
}
