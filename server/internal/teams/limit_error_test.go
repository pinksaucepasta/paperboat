package teams

import (
	"errors"
	"fmt"
	"testing"
)

// XE item 9: a team refusal names the limit and the numbers, and an oversized operation receipt is its own error,
// never reported as a team limit the user reached.
func TestTeamRefusalsNameTheirLimit(t *testing.T) {
	for kind, want := range map[string]string{
		"machine":          "team limit reached: 5 of 5 machines",
		"tunnel":           "team limit reached: 5 of 5 previews and tunnels",
		"terminal_session": "team limit reached: 5 of 5 shared terminal sessions",
		"member":           "team limit reached: 5 of 5 members",
		"invitation":       "team limit reached: 5 of 5 pending invitations",
	} {
		err := error(&LimitError{Kind: kind, Used: 5, Limit: 5})
		if err.Error() != want || !errors.Is(err, ErrLimit) {
			t.Errorf("%s: %q", kind, err)
		}
	}
	receipt := fmt.Errorf("%w: %d of %d bytes", ErrReceiptTooLarge, 1500, MaximumReceiptBytes)
	if errors.Is(receipt, ErrLimit) || !errors.Is(receipt, ErrReceiptTooLarge) {
		t.Fatalf("receipt size error: %v", receipt)
	}
}
