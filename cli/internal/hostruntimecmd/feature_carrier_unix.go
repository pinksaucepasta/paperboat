//go:build linux || darwin

package hostruntimecmd

func featureCarrierInstance() (string, error) { return carrierInstance("") }
