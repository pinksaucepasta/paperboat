//go:build linux || windows

package runtime

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/privateproxyconfig"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

func newPlatformPrivatePAC(stateRoot string) (privatepreviewproxy.PACConfigurator, error) {
	return privateproxyconfig.NewPlatformManager(stateRoot)
}
