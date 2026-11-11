package managedssh

type OpenSSHAliasTarget struct {
	Alias string
	User  string
	Port  uint16
	// AnyCaseUser lists the usual spellings of User in the fragment's `Match user`: a Windows machine's accounts are not
	// case-sensitive, OpenSSH's patterns are, and they offer no case-insensitive form (ssh_config(5) PATTERNS: only * ?
	// and !), so each spelling is listed. Linux and macOS users stay exact.
	AnyCaseUser bool
}
