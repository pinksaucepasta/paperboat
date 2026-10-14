//go:build windows

package hostruntimecmd

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"

func featureCarrierInstance() (string, error) {
	sid, err := currentWindowsSID()
	if err != nil {
		return "", err
	}
	return hostinstall.WindowsInstanceForSID(sid)
}
