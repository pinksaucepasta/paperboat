//go:build linux

package machineguard

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

// A replaced guard (every pb update) must not drop the listeners it holds. Each machine lease's listening socket
// is kept in systemd's descriptor store (FileDescriptorStore in the guard unit): the next guard process inherits
// it with LISTEN_FDS, and the nft rules stay in the kernel, so connects keep queueing in the socket's backlog
// while no guard runs. The daemons reconnect and re-acquire; the new guard hands them the inherited socket.

// leaseHandoff is true where a replaced guard keeps its leases (Linux under systemd).
func leaseHandoff() bool { return os.Getenv("NOTIFY_SOCKET") != "" }

const leaseFDPrefix = "pbl"

// leaseFDName carries everything the next guard needs to rebuild the lease. '|' cannot occur in any field and
// ':' (the LISTEN_FDNAMES separator) cannot occur in an IPv4 address.
func leaseFDName(l *guardedLease) string {
	return strings.Join([]string{leaseFDPrefix, l.uid, l.ip, strconv.Itoa(l.port), l.hostname}, "|")
}

func parseLeaseFDName(name string) (uid, ip string, port int, hostname string, err error) {
	parts := strings.Split(name, "|")
	if len(parts) != 5 || parts[0] != leaseFDPrefix {
		return "", "", 0, "", errors.New("not a machine guard lease descriptor")
	}
	port, err = strconv.Atoi(parts[3])
	if err != nil || port < 1 || port > 65535 {
		return "", "", 0, "", errors.New("invalid lease port")
	}
	if _, err = strconv.ParseUint(parts[1], 10, 32); err != nil || net.ParseIP(parts[2]) == nil || parts[4] == "" {
		return "", "", 0, "", errors.New("invalid lease descriptor")
	}
	return parts[1], parts[2], port, parts[4], nil
}

// notifyDescriptors sends one sd_notify datagram, with the descriptors as SCM_RIGHTS.
func notifyDescriptors(message string, files ...*os.File) error {
	notify := os.Getenv("NOTIFY_SOCKET")
	if notify == "" {
		return nil
	}
	// An unconnected datagram socket with the address on the call: Go refuses WriteMsgUnix on a connected one.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if strings.HasPrefix(notify, "@") {
		notify = "\x00" + notify[1:] // abstract socket
	}
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, file := range files {
			if fds[i], err = socketDescriptor(file); err != nil {
				return err
			}
		}
		oob = unix.UnixRights(fds...)
	}
	return unix.Sendmsg(fd, []byte(message), oob, &unix.SockaddrUnix{Name: notify}, 0)
}

// storeLease puts the lease's listening socket in the descriptor store. Gateway routes are not one socket and
// are rebuilt by their daemon instead.
func storeLease(l *guardedLease) error {
	tcp, ok := l.listener.(*net.TCPListener)
	if !ok || !leaseHandoff() {
		return nil
	}
	file, err := tcp.File()
	if err != nil {
		return err
	}
	defer file.Close()
	return notifyDescriptors("FDSTORE=1\nFDNAME="+leaseFDName(l), file)
}

// forgetLease removes the lease's socket from the descriptor store.
func forgetLease(l *guardedLease) {
	if _, ok := l.listener.(*net.TCPListener); ok && leaseHandoff() {
		_ = notifyDescriptors("FDSTOREREMOVE=1\nFDNAME=" + leaseFDName(l))
	}
}

// preservable reports whether stopping the guard leaves this lease's socket serving.
func preservable(l *guardedLease) bool {
	if route, ok := l.listener.(*gatewayRoute); ok {
		_, ok = route.gateway.listener.(*net.TCPListener)
		return ok && leaseHandoff()
	}
	_, ok := l.listener.(*net.TCPListener)
	return ok && leaseHandoff()
}

const gatewayFDPrefix = "pbg"

// gatewayFDName carries the browser port and the port the shared socket really listens on.
func gatewayFDName(s *sharedGateway) string {
	return strings.Join([]string{gatewayFDPrefix, strconv.Itoa(s.port), strconv.Itoa(s.listenPort())}, "|")
}

// storeGateway keeps a gateway port's shared listening socket in the descriptor store.
func storeGateway(s *sharedGateway) error {
	tcp, ok := s.listener.(*net.TCPListener)
	if !ok || !leaseHandoff() {
		return nil
	}
	file, err := tcp.File()
	if err != nil {
		return err
	}
	defer file.Close()
	return notifyDescriptors("FDSTORE=1\nFDNAME="+gatewayFDName(s), file)
}

func forgetGateway(s *sharedGateway) {
	if _, ok := s.listener.(*net.TCPListener); ok && leaseHandoff() {
		_ = notifyDescriptors("FDSTOREREMOVE=1\nFDNAME=" + gatewayFDName(s))
	}
}

// restoreGateway rebuilds a gateway port from its stored socket. Nobody accepts on it until a daemon registers a
// route (connects queue in its backlog); a placeholder lease keeps its nft rule meanwhile.
func (g *guardServer) restoreGateway(file *os.File, name string) {
	defer file.Close()
	parts := strings.Split(name, "|")
	if len(parts) != 3 {
		return
	}
	port, err1 := strconv.Atoi(parts[1])
	listenPort, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || !gatewayPortAllowed(port) || listenPort < 1 || listenPort > 65535 {
		return
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return
	}
	if addr, ok := listener.Addr().(*net.TCPAddr); !ok || addr.Port != listenPort || !addr.IP.Equal(net.ParseIP(splitdns.BrowserGatewayIP)) || g.gateways[port] != nil {
		listener.Close()
		return
	}
	gateway := g.newSharedGateway(port, listener, true)
	key := net.JoinHostPort(splitdns.BrowserGatewayIP, strconv.Itoa(port))
	g.leases[gatewayRouteKey(key, inheritedGatewayKey)] = &guardedLease{hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: port, uid: "0", listener: &gatewayRoute{gateway: gateway}}
}

// restoreInherited rebuilds the leases and gateway ports whose sockets the previous guard stored
// (sd_listen_fds); each has no owner until its daemon re-acquires it.
func (g *guardServer) restoreInherited() {
	files, names := inheritedDescriptors()
	g.restoreFrom(files, names)
}

func (g *guardServer) restoreFrom(files []*os.File, names []string) {
	var machineFiles []*os.File
	var machineNames []string
	for i, file := range files {
		switch {
		case i >= len(names):
			file.Close()
		case strings.HasPrefix(names[i], gatewayFDPrefix+"|"):
			g.restoreGateway(file, names[i])
		default:
			machineFiles, machineNames = append(machineFiles, file), append(machineNames, names[i])
		}
	}
	for _, lease := range leasesFromFiles(machineFiles, machineNames) {
		g.leases[net.JoinHostPort(lease.ip, strconv.Itoa(lease.port))] = lease
	}
}

func inheritedDescriptors() ([]*os.File, []string) {
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	count, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")
	if err != nil || count < 1 {
		return nil, nil
	}
	files := make([]*os.File, count)
	for i := range files {
		fd := 3 + i // SD_LISTEN_FDS_START
		unix.CloseOnExec(fd)
		files[i] = os.NewFile(uintptr(fd), "inherited-lease")
	}
	return files, names
}

// leasesFromFiles rebuilds a lease from each stored socket and its name; it closes the files.
func leasesFromFiles(files []*os.File, names []string) []*guardedLease {
	var leases []*guardedLease
	for i, file := range files {
		if i >= len(names) {
			file.Close()
			continue
		}
		uid, ip, port, hostname, err := parseLeaseFDName(names[i])
		if err != nil {
			file.Close()
			continue
		}
		listener, err := net.FileListener(file)
		file.Close() // FileListener dups
		if err != nil {
			continue
		}
		// The port is not checked: a machine lease listens on a high port (portRedirects).
		if addr, ok := listener.Addr().(*net.TCPAddr); !ok || !addr.IP.Equal(net.ParseIP(ip)) {
			listener.Close()
			continue
		}
		leases = append(leases, &guardedLease{hostname: hostname, ip: ip, port: port, uid: uid, listener: listener})
	}
	return leases
}
