package browserterminal

import "context"

// holdsOnly is a HoldsCarrier for unit tests: the named edge nodes hold the machine's carrier, any other does not.
func holdsOnly(nodes ...string) func(context.Context, Decision, string, string) (bool, error) {
	return func(_ context.Context, _ Decision, node, _ string) (bool, error) {
		for _, held := range nodes {
			if node == held {
				return true, nil
			}
		}
		return false, nil
	}
}
