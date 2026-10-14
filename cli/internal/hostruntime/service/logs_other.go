//go:build !darwin

package service

func prepareNativeServiceLogs(Config, []byte) error { return nil }

func (i *Installer) RemoveRetiredSessionOwnerLog() error { return nil }
