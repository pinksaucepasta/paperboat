package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"tailscale.com/tstest/integration"
)

func TestSystemOpenSSHOverNativeTailnet(t *testing.T) {
	if testing.Short() {
		t.Skip("real OpenSSH over native Tailnet integration")
	}
	for _, name := range []string{"ssh", "sshd", "ssh-keygen", "scp", "sftp", "nc"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("requires %s: %v", name, err)
		}
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sshdPort, identity, knownHosts, remoteFile := startNativeTestSSHD(t)

	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "ssh-cli")
	serverTLS, serverFingerprint := testTLS(t, "ssh-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_ssh", EndpointID: "cli_ssh", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::41"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_ssh", EndpointID: "machine_ssh", Role: "machine", MachineID: "machine_ssh", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::42"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()

	host, err := managedssh.NewHost(managedssh.HostConfig{MaxStreams: 8, ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.ReconcileTarget(ctx, 1, sshdPort); err != nil {
		t.Fatal(err)
	}
	serverUDP, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = serverOwner.Listen(ctx, dm.Regions[1], func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, _, acceptErr := session.AcceptAuthorized(serveCtx, func(_ context.Context, header streamauth.Header) (string, error) {
					if header.Consumer != "ssh" || header.Credential != "ssh-token" {
						return "", fmt.Errorf("SSH credential rejected")
					}
					return "grant_test", nil
				})
				if acceptErr != nil {
					return acceptErr
				}
				go func() { _, _ = host.Serve(serveCtx, 1, stream) }()
			}
		})
	}()
	descriptor := serverUDP.Address()
	var sessionMu sync.Mutex
	var active *native.Session
	adapter := tunnel.TailnetTerminalTunnel{DialSession: func(dialCtx context.Context, _ resolver.ConnectInfo) (*native.Session, error) {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if active != nil {
			return active, nil
		}
		session, dialErr := clientOwner.Dial(dialCtx, descriptor, "machine_ssh", peerquic.ClassInteractive)
		if dialErr == nil {
			active = session
		}
		return session, dialErr
	}}
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	info := resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{Auth: resolver.AuthTarget{Token: "ssh-token", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "grant_test"}}}
	proxyAddress, stopProxy := startNativeSSHProxy(t, ctx, adapter, info)
	defer stopProxy()
	proxyHost, proxyPort, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	proxyCommand := filepath.Join(t.TempDir(), "proxy")
	if err = os.WriteFile(proxyCommand, []byte("#!/bin/sh\nexec nc "+proxyHost+" "+proxyPort+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	common := []string{"-F", "/dev/null", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-i", identity, "-o", "ProxyCommand=" + proxyCommand}
	current, _ := user.Current()
	target := current.Username + "@paperboat"
	runOpenSSH(t, ctx, "ssh", nil, append(append([]string{}, common...), target, "printf paperboat-native-ssh")...)
	echoListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoListener.Close()
	go func() {
		for {
			connection, acceptErr := echoListener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _, _ = io.Copy(connection, connection); _ = connection.Close() }()
		}
	}()
	forwardProbe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forwardAddress := forwardProbe.Addr().String()
	_ = forwardProbe.Close()
	forwardCtx, stopForward := context.WithCancel(ctx)
	forwardArgs := append(append([]string{}, common...), "-o", "ExitOnForwardFailure=yes", "-N", "-L", forwardAddress+":"+echoListener.Addr().String(), target)
	forward := exec.CommandContext(forwardCtx, "ssh", forwardArgs...)
	var forwardOutput bytes.Buffer
	forward.Stdout, forward.Stderr = &forwardOutput, &forwardOutput
	if err = forward.Start(); err != nil {
		stopForward()
		t.Fatal(err)
	}
	forwarded := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, dialErr := net.DialTimeout("tcp4", forwardAddress, 50*time.Millisecond)
		if dialErr == nil {
			payload := []byte("paperboat-forward")
			_, writeErr := connection.Write(payload)
			response := make([]byte, len(payload))
			_, readErr := io.ReadFull(connection, response)
			_ = connection.Close()
			if writeErr == nil && readErr == nil && bytes.Equal(response, payload) {
				forwarded = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopForward()
	_ = forward.Wait()
	if !forwarded {
		t.Fatalf("OpenSSH forwarding failed: %s", forwardOutput.String())
	}
	upload := filepath.Join(t.TempDir(), "upload")
	if err = os.WriteFile(upload, []byte("paperboat-native-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	runOpenSSH(t, ctx, "scp", nil, append(append([]string{}, common...), upload, target+":"+remoteFile)...)
	download := filepath.Join(t.TempDir(), "download")
	runOpenSSH(t, ctx, "sftp", []byte("get "+remoteFile+" "+download+"\n"), append(append([]string{}, common...), "-b", "-", target)...)
	if content, readErr := os.ReadFile(download); readErr != nil || string(content) != "paperboat-native-file" {
		t.Fatalf("SFTP download=%q error=%v", content, readErr)
	}

	checkRemoteSSHOperations(t, ctx, common, target, echoListener.Addr().String(), func() {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if active != nil {
			_ = active.Close()
			active = nil
		}
	})

	// A new OpenSSH process after transport teardown proves reconnect without
	// attempting to replay an indeterminate SSH byte stream.
	sessionMu.Lock()
	_ = active.Close()
	active = nil
	sessionMu.Unlock()
	if output := runOpenSSH(t, ctx, "ssh", nil, append(append([]string{}, common...), target, "printf reconnected")...); string(output) != "reconnected" {
		t.Fatalf("reconnected SSH output=%q", output)
	}

	wrongHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err = os.WriteFile(wrongHosts, []byte("paperboat ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong := replaceOption(common, "UserKnownHostsFile=", "UserKnownHostsFile="+wrongHosts)
	command := exec.CommandContext(ctx, "ssh", append(wrong, target, "true")...)
	if output, wrongErr := command.CombinedOutput(); wrongErr == nil || !bytes.Contains(output, []byte("Host key verification failed")) {
		t.Fatalf("wrong host key was not rejected: err=%v output=%q", wrongErr, output)
	}

	clientConfig.Generation++
	clientConfig.Peers = nil
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	sessionMu.Lock()
	if active != nil {
		_ = active.Close()
		active = nil
	}
	sessionMu.Unlock()
	deniedCtx, deniedCancel := context.WithTimeout(ctx, 2*time.Second)
	defer deniedCancel()
	denied := exec.CommandContext(deniedCtx, "ssh", append(common, target, "true")...)
	if denied.Run() == nil {
		t.Fatal("revoked native SSH access succeeded")
	}
}

func startNativeTestSSHD(t *testing.T) (uint16, string, string, string) {
	t.Helper()
	root := t.TempDir()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	identity, hostKey := filepath.Join(root, "id_ed25519"), filepath.Join(root, "host_ed25519")
	for _, path := range []string{identity, hostKey} {
		if output, keyErr := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); keyErr != nil {
			t.Fatalf("ssh-keygen: %v: %s", keyErr, output)
		}
	}
	public, _ := os.ReadFile(identity + ".pub")
	authorized := filepath.Join(root, "authorized_keys")
	if err = os.WriteFile(authorized, public, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()
	configPath := filepath.Join(root, "sshd_config")
	config := fmt.Sprintf("ListenAddress 127.0.0.1\nPort %d\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nSubsystem sftp internal-sftp\nLogLevel ERROR\n", port, hostKey, filepath.Join(root, "sshd.pid"), authorized, current.Username)
	if err = os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sshdPath, err := exec.LookPath("sshd")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, sshdPath, "-D", "-e", "-f", configPath)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = command.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sshd readiness: %v: %s", dialErr, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	hostPublic, _ := os.ReadFile(hostKey + ".pub")
	fields := strings.Fields(string(hostPublic))
	knownHosts := filepath.Join(root, "known_hosts")
	if len(fields) < 2 || os.WriteFile(knownHosts, []byte("paperboat "+fields[0]+" "+fields[1]+"\n"), 0o600) != nil {
		t.Fatal("write known hosts")
	}
	return port, identity, knownHosts, filepath.Join(root, "remote-file")
}

func startNativeSSHProxy(t *testing.T, ctx context.Context, adapter tunnel.TailnetTerminalTunnel, info resolver.ConnectInfo, fixedOperation ...string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var operation atomic.Uint64
	go func() {
		for {
			local, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			operationID := fmt.Sprintf("operation_ssh_%d", operation.Add(1))
			if len(fixedOperation) == 1 {
				operationID = fixedOperation[0]
			}
			remote, dialErr := adapter.DialSSH(ctx, info, operationID)
			if dialErr != nil {
				_ = local.Close()
				continue
			}
			go func() {
				defer local.Close()
				defer remote.Close()
				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(remote, local)
					_ = remote.(tunnel.InputHalfCloser).CloseWrite()
					done <- struct{}{}
				}()
				go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return listener.Addr().String(), func() { _ = listener.Close() }
}

func runOpenSSH(t *testing.T, ctx context.Context, name string, input []byte, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, output)
	}
	return output
}

func replaceOption(values []string, prefix, replacement string) []string {
	result := append([]string{}, values...)
	for index := range result {
		if strings.HasPrefix(result[index], prefix) {
			result[index] = replacement
		}
	}
	return result
}
