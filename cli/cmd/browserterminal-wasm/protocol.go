package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
	"io"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

const (
	websocketSubprotocol = terminalprotocol.BrowserSubprotocol
	innerALPN            = terminalprotocol.BrowserSubprotocol

	identityEnvelopeMax = 8 << 10
	appStructuredMax    = protocol.MaxBrowserResponseFrame
	appBinaryMax        = protocol.MaxBinaryFrame
	websocketMessageMax = 256 << 10
	clientCertLifetime  = 10 * time.Minute

	appKindStructured byte = 1
	appKindBinary     byte = 2
)

var errInvalidPeerIdentity = errors.New("machine identity could not be verified")

type identityEnvelope struct {
	Certificate string `json:"certificate"`
	RootKeyID   string `json:"root_key_id"`
}

type machineIdentity struct {
	Certificate endpointidentity.Certificate
	RootPublic  ed25519.PublicKey
	Raw         []byte
}

type browserIdentity struct {
	PrivateKey  ed25519.PrivateKey
	TLS         tls.Certificate
	Fingerprint string
}

// newBrowserIdentity creates a key used for one browser attachment. The key is
// never persisted or shared with the control plane; only its SPKI fingerprint
// is used to bind the one-use server ticket to this TLS client certificate.
func newBrowserIdentity(now time.Time) (*browserIdentity, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate temporary browser key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("generate temporary browser certificate serial: %w", err)
	}
	start := now.UTC().Add(-time.Minute).Truncate(time.Second)
	end := start.Add(clientCertLifetime).Truncate(time.Second)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Paperboat temporary browser terminal"},
		NotBefore:    start,
		NotAfter:     end,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("create temporary browser certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("parse temporary browser certificate: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		zero(private)
		return nil, fmt.Errorf("encode temporary browser public key: %w", err)
	}
	fingerprint := sha256.Sum256(spki)
	return &browserIdentity{
		PrivateKey:  private,
		TLS:         tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf},
		Fingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]),
	}, nil
}

func verifyIdentityEnvelope(data []byte, rootPublic ed25519.PublicKey, rootKeyID, ownerAccountID, machineID string, now time.Time) (machineIdentity, error) {
	if len(data) == 0 || len(data) > identityEnvelopeMax || len(rootPublic) != ed25519.PublicKeySize || rootKeyID == "" || ownerAccountID == "" || machineID == "" {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	derivedKeyID := accountRootKeyID(rootPublic)
	if rootKeyID != derivedKeyID {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope identityEnvelope
	if err := decoder.Decode(&envelope); err != nil || envelope.Certificate == "" || envelope.RootKeyID != rootKeyID {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Certificate)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != envelope.Certificate {
		return machineIdentity{}, errInvalidPeerIdentity
	}
	certificate, err := endpointidentity.Verify(raw, rootPublic, endpointidentity.Expected{
		AccountID:  ownerAccountID,
		Role:       endpointidentity.RoleMachine,
		EndpointID: machineID,
	}, now)
	if err != nil {
		return machineIdentity{}, fmt.Errorf("%w: %w", errInvalidPeerIdentity, err)
	}
	return machineIdentity{Certificate: certificate, RootPublic: append(ed25519.PublicKey(nil), rootPublic...), Raw: raw}, nil
}

func accountRootKeyID(public ed25519.PublicKey) string {
	digest := sha256.Sum256(public)
	return "aek_" + hex.EncodeToString(digest[:])
}

func verifyMachineTLSLeaf(state tls.ConnectionState, identity machineIdentity, now time.Time) error {
	if len(state.PeerCertificates) != 1 || state.NegotiatedProtocol != innerALPN {
		return errInvalidPeerIdentity
	}
	leaf := state.PeerCertificates[0]
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+time.Minute {
		return errInvalidPeerIdentity
	}
	public, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(public, identity.Certificate.Claims.QUICPublicKey) {
		return errInvalidPeerIdentity
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		return errInvalidPeerIdentity
	}
	return nil
}

func tlsClientConfig(identity *browserIdentity, peer machineIdentity, now func() time.Time) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		NextProtos:             []string{innerALPN},
		Certificates:           []tls.Certificate{identity.TLS},
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true, // The machine uses a self-signed key certified by the independently pinned account root.
		VerifyConnection: func(state tls.ConnectionState) error {
			verified, err := endpointidentity.Verify(peer.Raw, peer.RootPublic, endpointidentity.Expected{
				AccountID:  peer.Certificate.Claims.AccountID,
				Role:       endpointidentity.RoleMachine,
				EndpointID: peer.Certificate.Claims.EndpointID,
				Generation: peer.Certificate.Claims.Generation,
			}, now())
			if err != nil || verified.Claims.Generation != peer.Certificate.Claims.Generation {
				return errInvalidPeerIdentity
			}
			return verifyMachineTLSLeaf(state, peer, now())
		},
	}
}

func writeApplicationFrame(w io.Writer, kind byte, payload []byte) error {
	maximum := appStructuredMax
	if kind == appKindBinary {
		maximum = appBinaryMax
	}
	if kind != appKindStructured && kind != appKindBinary || len(payload) == 0 || len(payload) > maximum {
		return errors.New("invalid terminal application frame")
	}
	// One TLS write avoids a separate encrypted record for the tiny header.
	wire := make([]byte, 5+len(payload))
	wire[0] = kind
	binary.BigEndian.PutUint32(wire[1:5], uint32(len(payload)))
	copy(wire[5:], payload)
	return writeFull(w, wire)
}

func readApplicationFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	kind := header[0]
	maximum := appStructuredMax
	if kind == appKindBinary {
		maximum = appBinaryMax
	} else if kind != appKindStructured {
		return 0, nil, errors.New("unknown terminal application frame kind")
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length == 0 || length > uint32(maximum) {
		return 0, nil, errors.New("terminal application frame length is invalid")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return kind, payload, nil
}

func writeFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func writeStructuredFrame(writer io.Writer, frame protocol.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return writeApplicationFrame(writer, appKindStructured, encoded)
}

func readStructuredFrame(reader io.Reader) (protocol.Frame, error) {
	kind, payload, err := readApplicationFrame(reader)
	if err != nil {
		return protocol.Frame{}, err
	}
	if kind != appKindStructured {
		return protocol.Frame{}, errors.New("expected a structured terminal frame")
	}
	return decodeStructuredFrame(payload)
}

func decodeStructuredFrame(payload []byte) (protocol.Frame, error) {
	if len(payload) > protocol.MaxBrowserResponseFrame {
		return protocol.Frame{}, errors.New("structured terminal frame is too large")
	}
	wire := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(wire, uint32(len(payload)))
	wire = append(wire, payload...)
	frame, err := protocol.ReadFrameLimit(bytes.NewReader(wire), protocol.MaxBrowserResponseFrame)
	// Only responses (inspector results) may exceed the ordinary frame limit.
	if err == nil && len(payload) > protocol.MaxStructuredFrame && frame.Type != "response" {
		return protocol.Frame{}, errors.New("structured terminal frame is too large")
	}
	return frame, err
}

// screenCheckpoint assembles one checkpoint from its frames, all at one boundary.
type screenCheckpoint struct {
	data     []byte
	sequence uint64
	started  bool
}

// add takes the next checkpoint frame and reports whether the checkpoint is complete.
func (c *screenCheckpoint) add(frame protocol.TerminalOutputFrame, streamID uint32) (bool, error) {
	if frame.Channel != protocol.TerminalScreenCheckpoint || frame.StreamID != streamID || len(frame.Data) == 0 || frame.Data[0] > 1 {
		return false, errors.New("invalid terminal screen checkpoint")
	}
	if !c.started {
		c.started, c.sequence = true, frame.StartSequence
	} else if frame.StartSequence != c.sequence {
		return false, errors.New("terminal screen checkpoint boundary changed")
	}
	if len(frame.Data)-1 > protocol.MaxTerminalScreenCheckpointBytes-len(c.data) {
		return false, errors.New("terminal screen checkpoint is too large")
	}
	c.data = append(c.data, frame.Data[1:]...)
	return frame.Data[0] == 1, nil
}

// The terminal's geometry bounds an attach response may name.
const (
	maximumColumns = 500
	maximumRows    = 200
)

func randomID(noun string) (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return noun + "_" + id.String(), nil
}

type terminalAttachState struct {
	StreamID         uint32
	AttachmentID     string
	AttachmentLease  uint64
	InputSequence    uint64
	Generation       uint64
	Columns          int
	Rows             int
	FirstAttachment  bool
	State            string
	ReplayFrom       uint64
	ReplayTo         uint64
	EarliestSequence uint64
	LatestSequence   uint64
	Recording        *wire.State // the session's recording at attach (REC before the first output)
}

func parseAttachResponse(payload []byte) (terminalAttachState, error) {
	var envelope struct {
		Result struct {
			StreamID        uint32 `json:"stream_id"`
			AttachmentID    string `json:"attachment_id"`
			AttachmentLease uint64 `json:"attachment_lease"`
			InputSequence   uint64 `json:"input_sequence"`
			FirstAttachment bool   `json:"first_attachment"`
			Session         struct {
				Snapshot struct {
					State      string `json:"state"`
					Generation uint64 `json:"generation"`
					Dimensions struct {
						Columns int `json:"columns"`
						Rows    int `json:"rows"`
					} `json:"dimensions"`
					EarliestSequence uint64      `json:"earliest_sequence"`
					LatestSequence   uint64      `json:"latest_sequence"`
					Recording        *wire.State `json:"recording"`
				} `json:"snapshot"`
				Replay struct {
					FromSequence uint64 `json:"from_sequence"`
					ToSequence   uint64 `json:"to_sequence"`
				} `json:"replay"`
			} `json:"session"`
		} `json:"result"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return terminalAttachState{}, errors.New("invalid terminal attach response")
	}
	result := envelope.Result
	if result.StreamID == 0 || result.AttachmentID == "" || result.AttachmentLease == 0 || result.Session.Snapshot.Generation == 0 || result.Session.Snapshot.Dimensions.Columns < 1 || result.Session.Snapshot.Dimensions.Columns > maximumColumns || result.Session.Snapshot.Dimensions.Rows < 1 || result.Session.Snapshot.Dimensions.Rows > maximumRows {
		return terminalAttachState{}, errors.New("terminal attach response is missing its stream or geometry")
	}
	if recording := result.Session.Snapshot.Recording; recording != nil && !recording.Valid() {
		return terminalAttachState{}, errors.New("terminal attach response has an invalid recording")
	}
	return terminalAttachState{
		StreamID: result.StreamID, AttachmentID: result.AttachmentID, AttachmentLease: result.AttachmentLease, InputSequence: result.InputSequence, Generation: result.Session.Snapshot.Generation,
		Columns: result.Session.Snapshot.Dimensions.Columns, Rows: result.Session.Snapshot.Dimensions.Rows,
		FirstAttachment: result.FirstAttachment,
		State:           result.Session.Snapshot.State, ReplayFrom: result.Session.Replay.FromSequence, ReplayTo: result.Session.Replay.ToSequence,
		EarliestSequence: result.Session.Snapshot.EarliestSequence, LatestSequence: result.Session.Snapshot.LatestSequence,
		Recording: result.Session.Snapshot.Recording,
	}, nil
}

// recordingJSON is the attach event's recording: the State as JSON, or null when not recording.
func (s terminalAttachState) recordingJSON() any {
	if s.Recording == nil {
		return nil
	}
	encoded, err := json.Marshal(s.Recording)
	if err != nil {
		return nil
	}
	return json.RawMessage(encoded)
}

func readScreenCheckpoint(stream io.Reader, streamID uint32) ([]byte, uint64, error) {
	checkpoint := screenCheckpoint{data: make([]byte, 0, 64<<10)}
	for {
		kind, payload, err := readApplicationFrame(stream)
		if err != nil || kind != appKindBinary {
			return nil, 0, errors.New("missing terminal screen checkpoint")
		}
		frame, err := protocol.DecodeTerminalOutput(payload)
		if err != nil {
			return nil, 0, errors.New("invalid terminal screen checkpoint")
		}
		if done, err := checkpoint.add(frame, streamID); err != nil {
			return nil, 0, err
		} else if done {
			return checkpoint.data, checkpoint.sequence, nil
		}
	}
}
