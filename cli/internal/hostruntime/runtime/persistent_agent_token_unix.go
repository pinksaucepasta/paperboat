//go:build darwin || linux

package runtime

import "os"

func privateAgentTokenFile(_ string, info os.FileInfo) bool { return info.Mode().Perm()&0077 == 0 }
