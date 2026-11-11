package hostinstall

import (
	"errors"
	"fmt"
)

// probeErrors keeps the first and the last failure of a readiness poll that ends by timeout: the last one alone hides
// what failed first (a dependency that failed at once and then kept failing the same way, or a different cause that the
// later retries buried).
type probeErrors struct{ first, last error }

func (p *probeErrors) note(err error) {
	if err == nil {
		return
	}
	if p.first == nil {
		p.first = err
	}
	p.last = err
}

// err is nil when nothing failed, the failure itself when first and last read the same, both otherwise.
func (p *probeErrors) err() error {
	if p.first == nil || p.first.Error() == p.last.Error() {
		return p.last
	}
	return errors.Join(fmt.Errorf("first failure: %w", p.first), fmt.Errorf("last failure: %w", p.last))
}
