package deviceguard

// UninstallResult distinguishes removed product integration from deliberately
// retained address isolation and identity needed for safe, repeatable recovery.
type UninstallResult struct {
	Removed  []string `json:"removed"`
	Retained []string `json:"retained"`
}
