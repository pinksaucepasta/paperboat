package daemonrpc

// DefaultWindowsNamedPipe is the standard IPC named pipe on Windows.
const DefaultWindowsNamedPipe = `\\.\pipe\paperboat-ipc`

// DefaultSocketAddress returns the current user's platform-specific daemon IPC
// endpoint. Producer and consumer use this same namespace.
func DefaultSocketAddress() string { return defaultSocketAddress() }
