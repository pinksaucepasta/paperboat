package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/recording"
)

// Team session recordings: the team recording key is made, shared and opened only inside this worker.
// Its private halves never leave it; the page gets public keys, sealed copies and decrypted text.

type recordingViewer struct {
	AccountID        string `json:"account_id"`
	SharingPublicKey string `json:"sharing_public_key"`
}

type recordingGrant struct {
	AccountID        string `json:"account_id"`
	SharingPublicKey string `json:"sharing_public_key"`
	Sealed           string `json:"sealed"`
}

type recordingSealed struct {
	Generation int64  `json:"generation"`
	Sealed     string `json:"sealed"`
}

type recordingLink struct {
	Generation     int64  `json:"generation"`
	SealedPrevious string `json:"sealed_previous"`
}

var errRecordingLocked = errors.New("Unlock ENV to use team recordings.")

// maxRecordingViewers bounds one grant request; maxRecordingHandles the replays open at once.
const (
	maxRecordingViewers = 512
	maxRecordingHandles = 8
)

func (e *engine) recordingOperation(r request) (any, error) {
	switch r.Action {
	case "recording-key-create":
		return e.recordingKeyCreate(r)
	case "recording-key-grant":
		return e.recordingKeyGrant(r)
	case "recording-open":
		return e.recordingOpen(r)
	case "recording-segment":
		reader := e.recordings[r.Handle]
		file, err := decode(r.File)
		if reader == nil || err != nil || len(file) == 0 || len(file) > recording.MaxSegmentFile {
			return nil, env.ErrInvalid
		}
		text, err := reader.Open(file)
		if err != nil {
			return nil, errors.New("A recording segment failed verification.")
		}
		return map[string]any{"text": string(text), "final": reader.Final()}, nil
	case "recording-close":
		if reader := e.recordings[r.Handle]; reader != nil {
			reader.Close()
			delete(e.recordings, r.Handle)
		}
		return map[string]bool{"closed": true}, nil
	}
	return nil, env.ErrInvalid
}

// recordingKeyCreate makes generation 1 of a team's recording key and seals its bundle to every viewer.
func (e *engine) recordingKeyCreate(r request) (any, error) {
	if r.TeamID == "" || len(r.Viewers) == 0 || len(r.Viewers) > maxRecordingViewers {
		return nil, env.ErrInvalid
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signingPublic, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	bundle := recording.Bundle{Private: private.Bytes(), Signing: signing}
	defer clear(bundle.Private)
	defer clear(bundle.Signing)
	grants, err := sealRecordingGrants(r.TeamID, 1, bundle, r.Viewers)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"key":    map[string]any{"generation": 1, "public_key": encoded(private.PublicKey().Bytes()), "signing_public_key": encoded(signingPublic)},
		"grants": grants,
	}, nil
}

// recordingKeyGrant opens this account's copy and seals the same generation for pending viewers.
func (e *engine) recordingKeyGrant(r request) (any, error) {
	if len(e.raw) == 0 {
		return nil, errRecordingLocked
	}
	sealed, err := decode(r.Sealed)
	if err != nil || r.TeamID == "" || r.Generation < 1 || len(r.Viewers) == 0 || len(r.Viewers) > maxRecordingViewers {
		return nil, env.ErrInvalid
	}
	bundle, err := recording.OpenBundle(e.keys.SharingPrivate, r.TeamID, r.Generation, sealed)
	if err != nil {
		return nil, errors.New("This browser's ENV keys cannot open the team recording key.")
	}
	defer clear(bundle.Private)
	defer clear(bundle.Signing)
	grants, err := sealRecordingGrants(r.TeamID, r.Generation, bundle, r.Viewers)
	if err != nil {
		return nil, err
	}
	return map[string]any{"grants": grants}, nil
}

func sealRecordingGrants(teamID string, generation int64, bundle recording.Bundle, viewers []recordingViewer) ([]recordingGrant, error) {
	grants := make([]recordingGrant, 0, len(viewers))
	for _, viewer := range viewers {
		public, err := decode(viewer.SharingPublicKey)
		if err != nil || viewer.AccountID == "" {
			return nil, env.ErrInvalid
		}
		sealed, err := recording.SealBundle(public, teamID, generation, bundle)
		if err != nil {
			return nil, err
		}
		grants = append(grants, recordingGrant{AccountID: viewer.AccountID, SharingPublicKey: viewer.SharingPublicKey, Sealed: encoded(sealed)})
	}
	return grants, nil
}

// recordingOpen opens the key generation that sealed one recording and keeps a reader for its segments.
func (e *engine) recordingOpen(r request) (any, error) {
	if len(e.raw) == 0 {
		return nil, errRecordingLocked
	}
	if r.ViewerKey == nil || r.TeamID == "" || r.RecordingID == "" || r.KeyGeneration < 1 || len(r.Chain) > 1024 {
		return nil, env.ErrInvalid
	}
	sealed, err := decode(r.ViewerKey.Sealed)
	if err != nil {
		return nil, env.ErrInvalid
	}
	chain := make([]recording.ChainLink, len(r.Chain))
	for i, link := range r.Chain {
		if chain[i].SealedPrevious, err = decode(link.SealedPrevious); err != nil {
			return nil, env.ErrInvalid
		}
		chain[i].Generation = link.Generation
	}
	teamKey, err := recording.OpenRecordingKey(e.keys.SharingPrivate, r.TeamID, r.ViewerKey.Generation, sealed, chain, r.KeyGeneration)
	if err != nil {
		return nil, errors.New("This browser's ENV keys cannot open this recording.")
	}
	if len(e.recordings) >= maxRecordingHandles {
		clear(teamKey)
		return nil, errors.New("Too many recordings are open. Close one first.")
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		clear(teamKey)
		return nil, err
	}
	handle := hex.EncodeToString(id)
	if e.recordings == nil {
		e.recordings = map[string]*recording.Reader{}
	}
	e.recordings[handle] = recording.NewReader(teamKey, r.RecordingID, r.KeyGeneration)
	return map[string]string{"handle": handle}, nil
}

// closeRecordings drops every open reader and its keys (lock, account change).
func (e *engine) closeRecordings() {
	for handle, reader := range e.recordings {
		reader.Close()
		delete(e.recordings, handle)
	}
}
