package preview

import "context"

type TokenSource interface {
	Token(context.Context) (string, error)
}

type ProofSource interface {
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}
