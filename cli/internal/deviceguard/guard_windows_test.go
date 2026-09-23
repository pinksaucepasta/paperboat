//go:build windows

package deviceguard

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/deviceguard/wfp"
	"golang.org/x/sys/windows"
)

const windowsAcceptanceIP = "127.123.254.231"
const windowsAcceptancePort = 46392
const windowsProbeRequest = `C:\Users\Public\paperboat-deviceguard-probe.request`
const windowsProbeResult = `C:\Users\Public\paperboat-deviceguard-probe.result`
const windowsGuardStop = `C:\Users\Public\paperboat-deviceguard-stop`

func TestWindowsMalformedPrefacesDoNotStopGuard(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_TEST") != "1" {
		t.Skip("requires task-owned installed Windows guard")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	malformed, err := winio.DialPipeAccessImpLevel(ctx, DefaultSocket, pipeClientAccess, winio.PipeImpLevelImpersonation)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifySystemPipe(malformed); err != nil {
		malformed.Close()
		t.Fatal(err)
	}
	_, _ = malformed.Write(bytes.Repeat([]byte{'x'}, len(pipeClientPreface)))
	malformed.Close()
	client, err := Connect(ctx, DefaultSocket)
	if err != nil {
		t.Fatal("valid control client after malformed preface", err)
	}
	defer client.Close()
	ip := netip.MustParseAddr("127.123.254.233")
	listener, err := client.Acquire(ctx, "prefaceguard.pprbt", ip, 46393)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	proxy := listener.(*windowsOwnedListener).windowsProxyListener
	badBroker, err := winio.DialPipeAccessImpLevel(ctx, proxy.path, pipeClientAccess, winio.PipeImpLevelImpersonation)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifySystemPipe(badBroker); err != nil {
		badBroker.Close()
		t.Fatal(err)
	}
	_, _ = badBroker.Write(bytes.Repeat([]byte{'y'}, len(pipeClientPreface)))
	badBroker.Close()
	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			conn.Close()
		}
		accepted <- acceptErr
	}()
	probe, err := net.DialTimeout("tcp4", "127.123.254.233:46393", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	if err = <-accepted; err != nil {
		t.Fatal("valid broker client after malformed preface", err)
	}
}

func TestWindowsProtectedGuardAcceptance(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_TEST") != "1" {
		t.Skip("requires task-owned installed Windows guard")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := Connect(ctx, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer os.WriteFile(windowsGuardStop, []byte("stop"), 0600)
	ip := netip.MustParseAddr(windowsAcceptanceIP)
	listener, err := client.Acquire(ctx, "taskguard.pprbt", ip, windowsAcceptancePort)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = client.ReplaceNames(ctx, map[string]netip.Addr{"taskguard.pprbt": ip}); err != nil {
		t.Fatal(err)
	}
	resolved, err := net.LookupHost("taskguard.pprbt")
	if err != nil || len(resolved) != 1 || resolved[0] != windowsAcceptanceIP {
		t.Fatalf("protected DNS=%v err=%v", resolved, err)
	}
	_ = os.Remove(windowsProbeResult)
	if err = os.WriteFile(windowsProbeRequest, []byte("probe"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(windowsProbeRequest)
	defer os.Remove(windowsProbeResult)
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		result, readErr := os.ReadFile(windowsProbeResult)
		if readErr == nil {
			if string(result) != "denied" {
				t.Fatalf("LocalSystem cross-owner probe result %q", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("LocalSystem cross-owner probe did not finish")
		}
	}
	accepted := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			accepted <- e
			return
		}
		defer conn.Close()
		buffer, readErr := io.ReadAll(conn)
		e = readErr
		if e == nil && !bytes.Equal(buffer, []byte("ping")) {
			e = errors.New("unexpected protected listener bytes")
		}
		if e == nil {
			_, e = conn.Write([]byte("pong"))
		}
		accepted <- e
	}()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort(windowsAcceptanceIP, "46392"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	_, err = conn.Read(reply)
	conn.Close()
	if err != nil || !bytes.Equal(reply, []byte("pong")) {
		select {
		case acceptErr := <-accepted:
			t.Fatalf("protected bytes=%q err=%v accept=%v", reply, err, acceptErr)
		case <-time.After(time.Second):
			t.Fatalf("protected bytes=%q err=%v accept did not finish", reply, err)
		}
	}
	if err = <-accepted; err != nil {
		t.Fatal(err)
	}
}
func TestWindowsProtectedHTTPSAcceptance(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_TEST") != "1" {
		t.Skip("requires task-owned installed Windows guard")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := Connect(ctx, DefaultSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ip := netip.MustParseAddr("127.123.254.232")
	listener, err := client.Acquire(ctx, "certguard.pprbt", ip, 443)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = client.ReplaceNames(ctx, map[string]netip.Addr{"certguard.pprbt": ip}); err != nil {
		t.Fatal(err)
	}
	bundle, err := client.Certificate(ctx, "certguard.pprbt")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13})
	done := make(chan error, 1)
	go func() {
		conn, e := tlsListener.Accept()
		if e == nil {
			_, e = conn.Write([]byte("trusted"))
			conn.Close()
		}
		done <- e
	}()
	conn, err := tls.Dial("tcp4", "127.123.254.232:443", &tls.Config{ServerName: "certguard.pprbt", MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(data) != "trusted" {
		t.Fatalf("trusted HTTPS=%q err=%v", data, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestWindowsProtectedGuardSystemDenied(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_SYSTEM_PROBE") != "1" {
		t.Skip("requires LocalSystem probe")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil || user == nil || !user.User.Sid.Equals(system) {
		t.Fatal("probe is not LocalSystem")
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort(windowsAcceptanceIP, "46392"), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("LocalSystem reached another SID's protected listener")
	}
}
func TestWindowsProtectedGuardPersistentDeny(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_DENY") != "1" {
		t.Skip("requires installed Windows guard policy")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", "46392"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort(windowsAcceptanceIP, "46392"), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("withdrawn protected address reached wildcard listener")
	}
	retained, err := net.DialTimeout("tcp4", net.JoinHostPort("127.100.254.230", "46392"), 500*time.Millisecond)
	if err == nil {
		retained.Close()
		t.Fatal("retained previous range reached wildcard listener")
	}
}
func TestWindowsRemoveOwnedGuardPolicy(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_CLEANUP") != "1" {
		t.Skip("cleanup only")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err = wfp.Remove([]wfp.Lease{{IP: windowsAcceptanceIP, SID: user.User.Sid.String()}, {IP: "127.123.254.232", SID: user.User.Sid.String()}}); err != nil {
		t.Fatal(err)
	}
}
func TestWindowsGuardServerProcess(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_SERVER") != "1" {
		t.Skip("task-owned LocalSystem server only")
	}
	t.Setenv("PAPERBOAT_WINDOWS_GUARD_TRACE", "1")
	_ = os.Remove(windowsGuardStop)
	defer os.Remove(windowsGuardStop)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(windowsGuardStop); err == nil {
				cancel()
				return
			}
			if _, err := os.Stat(windowsProbeRequest); err == nil {
				conn, dialErr := net.DialTimeout("tcp4", net.JoinHostPort(windowsAcceptanceIP, "46392"), 500*time.Millisecond)
				if dialErr == nil {
					conn.Close()
					_ = os.WriteFile(windowsProbeResult, []byte("allowed"), 0600)
				} else {
					_ = os.WriteFile(windowsProbeResult, []byte("denied"), 0600)
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := Serve(ctx, Config{ConfigureResolver: true, LoopbackCIDR: "127.123.0.0/16", ProtectedLoopbackCIDRs: []string{"127.100.0.0/16"}}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWindowsProtectedDirectoryACLProbe(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_GUARD_ACL_PROBE") != "1" {
		t.Skip("requires LocalSystem ACL probe")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil || user == nil || !user.User.Sid.Equals(system) {
		t.Fatal("ACL probe is not LocalSystem")
	}
	if err = protectedDirectory(DefaultStateDir, 0700); err != nil {
		t.Fatalf("restored read-only ancestor rejected: %v", err)
	}
	foreign := `C:\ProgramData\Paperboat\DeviceGuardForeignProbe`
	if err = os.Mkdir(foreign, 0700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	defer os.RemoveAll(foreign)
	users, _ := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_GROUP, TrusteeValue: windows.TrusteeValueFromSID(users)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(foreign, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(foreign, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err = protectedDirectory(foreign, 0700); err == nil {
		t.Fatal("writable foreign ancestor accepted")
	}
	after, getErr := windows.GetNamedSecurityInfo(foreign, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if getErr != nil || before.String() != after.String() {
		t.Fatalf("rejected foreign ACL was mutated: %v", getErr)
	}
}
