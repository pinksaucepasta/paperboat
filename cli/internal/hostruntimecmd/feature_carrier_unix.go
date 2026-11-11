//go:build linux || darwin

package hostruntimecmd

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"

func featureCarrierInstance() (string, error) { return carrierInstance("") }

// carrierPairedWithHostd is always true here: generations hand the fixed hostd socket over, so the carrier serving
// the network always reaches the serving hostd.
func carrierPairedWithHostd(hostinstall.CarrierConfig, string) bool { return true }
