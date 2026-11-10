package httpapi

import (
	"fmt"
	"sync/atomic"
)

var proofTestNonceCounter atomic.Int64

// proofTestNonce is a fresh well-formed one-use nonce: every machine and helper proof must carry one.
func proofTestNonce() string { return fmt.Sprintf("n0nce-test-%012d", proofTestNonceCounter.Add(1)) }
