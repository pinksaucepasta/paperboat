//go:build !windows

package service

func windowsSessionOwnerController() (Controller, error) { return nil, ErrUnsupportedPlatform }

func windowsConfiguredServiceName(config Config) string {
	if config.Kind == SessionOwnerKind {
		return "PaperboatSessionOwner-" + config.Instance + "-" + config.Generation
	}
	if config.Kind == CarrierKind && config.Platform == "windows" {
		return "PaperboatCarrier-" + config.Instance + "-" + config.Generation
	}
	if config.Kind == HostdKind && config.Platform == "windows" {
		return "PaperboatHostd-" + config.Instance + "-" + config.Generation
	}
	return windowsServiceName(config.Kind, config.Instance)
}
