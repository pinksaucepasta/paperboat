package previewattachment

import "strings"

// SecondaryOperationSuffix marks the operation of a preview's second carrier attachment: the
// lease's create operation plus this suffix. The second attachment is an ordinary attachment (own
// generation, readiness and release) held on another edge of the workspace's set, so the preview
// keeps serving when one edge dies. Requests without the suffix are unchanged.
const SecondaryOperationSuffix = ".b"

// SplitOperationSlot returns the lease's create operation and the edge slot (0 primary, 1 secondary)
// an attachment operation id stands for.
func SplitOperationSlot(operationID string) (string, int) {
	if base, ok := strings.CutSuffix(operationID, SecondaryOperationSuffix); ok && base != "" {
		return base, 1
	}
	return operationID, 0
}
