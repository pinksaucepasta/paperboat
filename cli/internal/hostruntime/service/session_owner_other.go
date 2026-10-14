//go:build !windows

package service

func windowsSessionOwnerController() (Controller, error) { return nil, ErrUnsupportedPlatform }

func windowsConfiguredServiceName(config Config) string {
	if config.Kind == SessionOwnerKind {
		return "PaperboatSessionOwner-" + config.Instance + "-" + config.SessionOwnerGeneration
	}
	return windowsServiceName(config.Kind, config.Instance)
}
