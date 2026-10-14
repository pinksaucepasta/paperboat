package auth

// ConfigureAuthorizationEncryptionKey configures encrypted device token recovery.
// It must be called before serving authorization requests.
func (s *DeviceService) ConfigureAuthorizationEncryptionKey(key string) {
	s.authorizationEncryptionKey = key
}
