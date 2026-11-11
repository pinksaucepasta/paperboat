// Package peerconsumer owns the consumer names and capability projections of native application streams.
package peerconsumer

// NetworkCapability is the signed network scope used by both ends of a stream.
// Management-only operations and unknown consumers never receive network access.
func NetworkCapability(consumer string) string {
	switch consumer {
	case "terminal", "exec", "config_compare", "recording", "inspector", "codex":
		return consumer
	case "ssh":
		return "managed_ssh"
	case "file_transfer", "file_transfer_key":
		return "file_transfer"
	case "private_http", "private_tcp":
		return "private_access"
	default:
		return ""
	}
}
