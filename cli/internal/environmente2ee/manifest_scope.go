package environmente2ee

import (
	"bytes"
	"encoding/binary"
	"io"
	"sort"
	"unicode/utf8"
)

func encodeScope(values map[string][]byte) ([]string, []byte, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	if validateNames(names) != nil {
		return nil, nil, ErrInvalid
	}
	entries := make([]any, len(names))
	total := 0
	for i, name := range names {
		value := values[name]
		if len(value) > MaximumValueBytes || bytes.IndexByte(value, 0) >= 0 || !utf8.Valid(value) {
			return nil, nil, ErrInvalid
		}
		total += len(name) + len(value)
		entries[i] = []any{name, value}
	}
	raw, err := encode([]any{"paperboat.environment.scope-plaintext", uint64(1), entries})
	if err != nil || len(raw) > MaximumScopeBytes || total > MaximumScopeBytes {
		return nil, nil, ErrInvalid
	}
	return names, raw, nil
}

func decodeScope(raw []byte) (map[string][]byte, []string, error) {
	var v any
	if decodeCanonical(raw, MaximumScopeBytes, &v) != nil {
		return nil, nil, ErrInvalid
	}
	a, e := array(v, 3)
	if e != nil || requireDomain(a, "paperboat.environment.scope-plaintext", 1) != nil {
		return nil, nil, ErrInvalid
	}
	entries, e := arrayAny(a[2])
	if e != nil || len(entries) > MaximumVariables {
		return nil, nil, ErrInvalid
	}
	values := make(map[string][]byte, len(entries))
	names := make([]string, len(entries))
	for i, x := range entries {
		p, e := array(x, 2)
		if e != nil {
			return nil, nil, ErrInvalid
		}
		name, e1 := text(p[0])
		value, e2 := bytesValue(p[1], -1)
		if e1 != nil || e2 != nil || len(value) > MaximumValueBytes || bytes.IndexByte(value, 0) >= 0 || !utf8.Valid(value) {
			return nil, nil, ErrInvalid
		}
		names[i] = name
		values[name] = value
	}
	if validateNames(names) != nil {
		return nil, nil, ErrInvalid
	}
	return values, names, nil
}

var paddingBuckets = []int{1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 512 << 10}

func padPlaintext(random io.Reader, plain []byte) ([]byte, error) {
	needed := 4 + len(plain)
	bucket := 0
	for _, size := range paddingBuckets {
		if needed <= size {
			bucket = size
			break
		}
	}
	if bucket == 0 {
		return nil, ErrInvalid
	}
	out := make([]byte, bucket)
	binary.BigEndian.PutUint32(out, uint32(len(plain)))
	copy(out[4:], plain)
	if _, err := io.ReadFull(random, out[4+len(plain):]); err != nil {
		return nil, err
	}
	return out, nil
}

func unpadPlaintext(padded []byte) ([]byte, error) {
	if !containsInt(paddingBuckets, len(padded)) || len(padded) < 4 {
		return nil, ErrInvalid
	}
	length := int(binary.BigEndian.Uint32(padded))
	if length <= 0 || length > len(padded)-4 || length > MaximumScopeBytes {
		return nil, ErrInvalid
	}
	return cloneBytes(padded[4 : 4+length]), nil
}

func validCiphertextLength(n int) bool {
	for _, b := range paddingBuckets {
		if n == b+16 {
			return true
		}
	}
	return false
}
