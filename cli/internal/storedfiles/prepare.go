// Package storedfiles encrypts pending files for a team's bucket and receives them
// through the same bounded, durable owner as online file transfers.
package storedfiles

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/diskspace"
	"github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	receiver "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// PartPlainBytes leaves room for AES-GCM's nonce and tag inside the bucket limit.
const PartPlainBytes = teambucket.MaxObjectBytes - 12 - 16

var ErrInvalid = errors.New("stored file metadata or ciphertext is invalid")

// Prepared holds only ciphertext, in a private temporary directory. Close removes
// it on every sender exit, including an interrupted upload.
type Prepared struct {
	Parts []wire.Part
	root  string
}

func (p *Prepared) Close() error {
	if p == nil || p.root == "" {
		return nil
	}
	err := os.RemoveAll(p.root)
	if err == nil {
		p.root = ""
	}
	return err
}

func (p *Prepared) ReadPart(index int) ([]byte, error) {
	if p == nil || p.root == "" || index < 0 || index >= len(p.Parts) {
		return nil, ErrInvalid
	}
	data, err := os.ReadFile(filepath.Join(p.root, fmt.Sprintf("%06d.part", index)))
	if err != nil {
		return nil, err
	}
	part := p.Parts[index]
	if int64(len(data)) != part.Bytes || digest(data) != part.SHA256 {
		return nil, ErrInvalid
	}
	return data, nil
}

func newCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrInvalid
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func partAD(id, manifest string, part wire.Part) []byte {
	data, _ := json.Marshal([]any{"paperboat.files.stored-part", 1, id, manifest, part.Index, part.File, part.Offset})
	return data
}

func validContext(id, manifest string) bool {
	hash, err := hex.DecodeString(manifest)
	return id != "" && len(id) <= 256 && err == nil && len(hash) == 32 && hex.EncodeToString(hash) == manifest
}

// Prepare snapshots the measured sources as independently authenticated encrypted
// parts. Hashing the bytes again detects a source changed since file selection.
func Prepare(ctx context.Context, id, manifest string, sources []filetransfer.Source, key []byte, directory string) (_ *Prepared, resultErr error) {
	if ctx == nil || !validContext(id, manifest) || !filepath.IsAbs(directory) {
		return nil, ErrInvalid
	}
	if err := filetransfer.ValidateSources(sources, filetransfer.Limits{MaxFileBytes: receiver.AutomaticTransferLimit, MaxBatchFiles: receiver.MaxBatchFiles, MaxBatchBytes: receiver.AutomaticTransferLimit}); err != nil {
		return nil, err
	}
	files := make([]wire.File, len(sources))
	for i, source := range sources {
		files[i] = wire.File{Basename: source.Basename, Size: source.Size, SHA256: hex.EncodeToString(source.SHA256[:])}
	}
	if ManifestDigest(files) != manifest {
		return nil, ErrInvalid
	}
	aead, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	var needed uint64
	for _, source := range sources {
		count := max(int64(1), (source.Size+PartPlainBytes-1)/PartPlainBytes)
		needed += uint64(source.Size + count*int64(aead.NonceSize()+aead.Overhead()))
	}
	free, err := diskspace.Available(directory)
	if err != nil {
		return nil, fmt.Errorf("measure space for encrypted files: %w", err)
	}
	if needed > free {
		return nil, fmt.Errorf("encrypted files need %d bytes; %d bytes are free", needed, free)
	}
	root, err := os.MkdirTemp(directory, "paperboat-stored-")
	if err != nil {
		return nil, err
	}
	p := &Prepared{root: root}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, p.Close())
		}
	}()
	for index, source := range sources {
		if err := p.prepareFile(ctx, aead, id, manifest, index, source); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Prepared) prepareFile(ctx context.Context, aead cipher.AEAD, id, manifest string, fileIndex int, source filetransfer.Source) error {
	if _, err := source.Reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	for offset := int64(0); offset < source.Size || offset == 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := min(int64(PartPlainBytes), source.Size-offset)
		plain := make([]byte, int(size))
		if _, err := io.ReadFull(source.Reader, plain); err != nil {
			clear(plain)
			return fmt.Errorf("read stored file: %w", err)
		}
		_, _ = hash.Write(plain)
		part := wire.Part{Index: len(p.Parts), File: fileIndex, Offset: offset}
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			clear(plain)
			return err
		}
		data := aead.Seal(nonce, nonce, plain, partAD(id, manifest, part))
		clear(plain)
		part.Bytes, part.SHA256 = int64(len(data)), digest(data)
		if err := os.WriteFile(filepath.Join(p.root, fmt.Sprintf("%06d.part", part.Index)), data, 0o600); err != nil {
			return err
		}
		p.Parts = append(p.Parts, part)
		offset += size
		if source.Size == 0 {
			break
		}
	}
	var extra [1]byte
	n, err := source.Reader.Read(extra[:])
	if n != 0 || err != io.EOF || hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(source.SHA256[:]) {
		return errors.New("the source changed while preparing stored files; select it again")
	}
	return nil
}

// ManifestDigest is the exact ordered file manifest protected by every key seal.
func ManifestDigest(files []wire.File) string {
	raw, err := json.Marshal(files)
	if err != nil {
		return ""
	}
	return digest(raw)
}
