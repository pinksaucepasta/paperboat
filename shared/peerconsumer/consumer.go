// Package peerconsumer owns the native application stream consumers: their names, the capabilities each one maps
// to, and the rules that differ between them. Every allowlist and capability map of the client, the host, the local
// daemon API and the server's usage accounting reads this table, so a consumer is added or removed in one place.
package peerconsumer

// Consumer names carried in stream headers, local daemon requests and native usage reports.
const (
	Terminal      = "terminal"
	Exec          = "exec"
	SSH           = "ssh"
	ConfigCompare = "config_compare"
	Recording     = "recording"
	FileTransfer  = "file_transfer"
	Inspector     = "inspector"
	PrivateTCP    = "private_tcp"
	PrivateHTTP   = "private_http"
	// HealthProbe opens a native session and no stream: no credential, no scope, no usage.
	HealthProbe = "health_probe"
)

type consumer struct {
	network  string // signed network scope capability (peernetwork); "" = no network access
	protocol string // host protocol capability the stream's credential must grant
	daemon   bool   // opened through the local daemon's peer stream API
	pending  bool   // the daemon may fill its credential (localdaemon IssuePeerStream)
	offline  bool   // the host may admit it from its own signed rules while Paperboat is unreachable
	oneShot  bool   // reads one bounded result: never resumed after a transport loss
}

var consumers = map[string]consumer{
	Terminal:      {network: "terminal", protocol: "terminal.v1", daemon: true, offline: true},
	Exec:          {network: "exec", protocol: "exec.v1", daemon: true, pending: true, offline: true},
	SSH:           {network: "managed_ssh", protocol: "ssh.v1", daemon: true, pending: true, offline: true},
	ConfigCompare: {network: "config_compare", protocol: "config.compare.v1", daemon: true, oneShot: true},
	Recording:     {network: "recording", protocol: "recording.v1", daemon: true, oneShot: true},
	FileTransfer:  {network: "file_transfer", protocol: "file-transfer.v1"},
	Inspector:     {network: "inspector", protocol: "inspector.v1"},
	PrivateTCP:    {network: "private_access", protocol: "private.access.v1"},
	PrivateHTTP:   {network: "private_access", protocol: "private.access.v1"},
	HealthProbe:   {daemon: true},
}

// NetworkCapability is the signed network scope used by both ends of a stream; "" for an unknown consumer or one
// that opens no stream. A stream with network access is a metered stream.
func NetworkCapability(name string) string { return consumers[name].network }

// ProtocolCapability is the host capability a stream's credential must grant; "" refuses the stream.
func ProtocolCapability(name string) string { return consumers[name].protocol }

// Daemon reports whether the local daemon's peer stream API opens this consumer.
func Daemon(name string) bool { return consumers[name].daemon }

// Pending reports whether the local daemon fills this consumer's credential itself.
func Pending(name string) bool { return consumers[name].pending }

// Offline reports whether a host admits this consumer from its own signed rules, without a credential.
func Offline(name string) bool { return consumers[name].offline }

// Resumable reports whether a stream of this consumer may be resumed on a new transport.
func Resumable(name string) bool { c, ok := consumers[name]; return ok && !c.oneShot }
