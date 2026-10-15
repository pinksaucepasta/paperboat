package peeridentity

type CertificateDocument struct {
	Version                int    `json:"version"`
	AccountID              string `json:"account_id"`
	KeyID                  string `json:"key_id"`
	EndpointID             string `json:"endpoint_id"`
	Role                   string `json:"role"`
	Generation             uint64 `json:"generation"`
	Serial                 uint64 `json:"serial"`
	IssuedAt               string `json:"issued_at"`
	ExpiresAt              string `json:"expires_at"`
	Certificate            string `json:"certificate"`
	CertificateFingerprint string `json:"certificate_fingerprint"`
}

type BootstrapDocument struct {
	KeyID       string               `json:"key_id"`
	TrustedKeys []TrustedKeyDocument `json:"trusted_keys"`
	Certificate CertificateDocument  `json:"certificate"`
}

type BootstrapRequestDocument struct {
	RootPublicKey string              `json:"root_public_key"`
	Certificate   CertificateDocument `json:"certificate"`
}

type RootDocument struct {
	Version     int                  `json:"version"`
	TrustedKeys []TrustedKeyDocument `json:"trusted_keys"`
}

type TrustedKeyDocument struct {
	KeyID       string `json:"key_id"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	Generation  uint64 `json:"generation"`
}

type EnrollmentDocument struct {
	RequestID     string `json:"request_id"`
	EndpointID    string `json:"endpoint_id"`
	Role          string `json:"role"`
	State         string `json:"state"`
	Generation    uint64 `json:"generation"`
	QUICPublicKey string `json:"quic_public_key"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	SafetyCode    string `json:"safety_code"`
}

type CLIEndpointRequestDocument struct {
	OperationID   string `json:"operation_id"`
	EndpointID    string `json:"endpoint_id"`
	Generation    uint64 `json:"generation"`
	QUICPublicKey string `json:"quic_public_key"`
}
