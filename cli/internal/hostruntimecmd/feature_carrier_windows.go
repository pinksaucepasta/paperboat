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

// carrierPairedWithHostd reports whether config's carrier serves the hostd generation at endpoint: each Windows hostd
// generation has its own pipes and pairs with the carrier generation staged for it.
func carrierPairedWithHostd(config hostinstall.CarrierConfig, endpoint string) bool {
	return config.HostdEndpoint == endpoint
}
