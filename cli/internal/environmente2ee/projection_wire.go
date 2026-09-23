package environmente2ee

type ProjectionBundle struct {
	Schema                 string `json:"schema"`
	AccountID              string `json:"account_id"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
	WriterPublic           string `json:"writer_public"`
	FenceGeneration        uint64 `json:"fence_generation"`
	SelectionGeneration    uint64 `json:"selection_generation"`
	ProjectionRevision     uint64 `json:"projection_revision"`
	DocumentID             string `json:"document_id"`
	Envelope               string `json:"envelope"`
	State                  string `json:"state"`
}
