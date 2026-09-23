package environmente2ee

import (
	"context"
	"golang.org/x/crypto/argon2"
	"unicode/utf8"
)

func passwordRoot(ctx context.Context, password, salt []byte) ([]byte, error) {
	if ctx == nil || len(password) == 0 || len(password) > 1024 || !utf8.Valid(password) || len(salt) != 16 {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case vaultKDFSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-vaultKDFSlot }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := argon2.IDKey(password, salt, 3, 64*1024, 4, 32)
	if err := ctx.Err(); err != nil {
		clear(root)
		return nil, err
	}
	return root, nil
}
