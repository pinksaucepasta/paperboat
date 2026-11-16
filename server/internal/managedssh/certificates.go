package managedssh

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"golang.org/x/crypto/ssh"
)

// CertificateConfig configures certificates. EncryptionKey seals CA private keys at rest; Validity is how long a certificate lets a client log
// in, so how long clients keep starting sessions while the control plane is unreachable. Revocation does not wait
// for it (TrustedUserCASet.RevokedKeyIDs).
type CertificateConfig struct {
	EncryptionKey string
	Validity      time.Duration
}

// CAPropagation is how long a pending CA is trusted by hosts before it signs. It is longer than the host's fallback
// refresh of its trusted CAs (cli hostruntime managedSSHKeyFallback, 10 minutes), so every online host trusts the
// new CA before any certificate it signs is presented.
const CAPropagation = 15 * time.Minute

var ErrRotationInvalid = errors.New("managed SSH CA rotation request is invalid")

type UserCA struct {
	ID                   string
	AccountID            string
	State                string
	PublicKey            string
	PrivateKeyCiphertext []byte
	CreatedAt            time.Time
	StateChangedAt       time.Time
}

// NewUserCA is a freshly generated CA the repository stores only when the account needs one.
type NewUserCA struct {
	ID                   string
	PublicKey            string
	PrivateKeyCiphertext []byte
}

type IssueCertificateRequest struct {
	UserID             string
	CLIClientSessionID string
	UserMachineID      string
	MachineGeneration  uint64
	PublicKey          string
	Now                time.Time
	// ValidBefore is set by the service: the repository records the holder until then.
	ValidBefore time.Time
}

// CertificateAuthorization is what the repository authorized for one certificate.
type CertificateAuthorization struct {
	OSUser string
	CA     UserCA
}

// TrustedUserCASet is what a host trusts: the CA keys, the principal a certificate must carry, and the key IDs of
// still-valid certificates it must refuse.
type TrustedUserCASet struct {
	Keys          []string
	Principal     string
	RevokedKeyIDs []string
}

// MachinePrincipal is the principal a machine's CA line requires (`cert-authority,principals=`): a certificate for
// one machine is refused by every other. It never changes, so revoking one user touches no one else's certificate.
func MachinePrincipal(machineID string) string { return "pb:" + machineID }

// CertificateKeyID is the key ID of every certificate one CLI session gets for one machine. Revocation names it: the
// host refuses it in its bridge and in an OpenSSH KRL (RevokedKeys, `ssh-keygen -k`), like a Teleport lock on a
// session (~/ref/teleport lib/services/lock.go LockTargetsFromTLSIdentity, enforced by lib/services/watcher.go
// LockWatcher), until the certificates expire.
func CertificateKeyID(userID, sessionID, machineID string) string {
	return "paperboat:" + userID + ":" + sessionID + ":" + machineID
}

type Certificate struct {
	Certificate string
	OSUser      string
	ValidBefore time.Time
}

type UserCAsRequest struct {
	ActorUserID       string
	UserMachineID     string
	MachineGeneration uint64
	Now               time.Time
}

type RotateUserCARequest struct {
	AccountID string
	Now       time.Time
	// Propagation and Validity are CAPropagation and the certificate validity; tests shorten them.
	Propagation time.Duration
	Validity    time.Duration
}

// IssueCertificate signs the client's public key for the exact machine: principals = the machine's target OS user and
// its machine principal, usable only through the host's loopback bridge (source-address), valid for the
// configured hours. Like
// Teleport's keygen.GenerateUserCertWithoutValidation (~/ref/teleport lib/auth/keygen/keygen.go), ValidAfter is a
// minute in the past for clock skew.
func (s *Service) IssueCertificate(ctx context.Context, request IssueCertificateRequest) (Certificate, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(request.UserID) || !bounded(request.CLIClientSessionID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 || request.Now.IsZero() {
		return Certificate{}, wire.ErrInvalid
	}
	parsed, err := wire.ParsePublicKey(request.PublicKey, true)
	if err != nil {
		return Certificate{}, err
	}
	public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(parsed.PublicKey))
	if err != nil {
		return Certificate{}, wire.ErrInvalid
	}
	candidate, err := s.newUserCA()
	if err != nil {
		return Certificate{}, err
	}
	now := request.Now.UTC()
	validBefore := now.Add(s.certificates.Validity)
	request.ValidBefore = validBefore
	authorization, err := s.repository.CertificateAuthority(ctx, request, candidate)
	if err != nil {
		return Certificate{}, err
	}
	signer, err := s.caSigner(authorization.CA)
	if err != nil {
		return Certificate{}, err
	}
	var serial [8]byte
	if _, err := s.random.Read(serial[:]); err != nil {
		return Certificate{}, err
	}
	certificate := &ssh.Certificate{
		Key:             public,
		Serial:          binary.BigEndian.Uint64(serial[:]),
		CertType:        ssh.UserCert,
		KeyId:           CertificateKeyID(request.UserID, request.CLIClientSessionID, request.UserMachineID),
		ValidPrincipals: []string{authorization.OSUser, MachinePrincipal(request.UserMachineID)},
		ValidAfter:      uint64(now.Add(-time.Minute).Unix()),
		ValidBefore:     uint64(validBefore.Unix()),
		Permissions: ssh.Permissions{
			CriticalOptions: map[string]string{"source-address": wire.CertificateSourceAddress},
			// The extensions ssh-keygen grants by default: a plain authorized key allowed all of these.
			Extensions: map[string]string{"permit-X11-forwarding": "", "permit-agent-forwarding": "", "permit-port-forwarding": "", "permit-pty": "", "permit-user-rc": ""},
		},
	}
	if err := certificate.SignCert(s.random, signer); err != nil {
		return Certificate{}, err
	}
	return Certificate{Certificate: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(certificate))), OSUser: authorization.OSUser, ValidBefore: validBefore}, nil
}

// TrustedUserCAs are what a host trusts: every CA of the machine's owner account (pending, active and retiring),
// created on first use, the machine's principal and the key IDs of still-valid certificates whose holders lost access.
func (s *Service) TrustedUserCAs(ctx context.Context, request UserCAsRequest) (TrustedUserCASet, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 || request.Now.IsZero() {
		return TrustedUserCASet{}, wire.ErrInvalid
	}
	candidate, err := s.newUserCA()
	if err != nil {
		return TrustedUserCASet{}, err
	}
	cas, revoked, err := s.repository.UserCAs(ctx, request, candidate)
	if err != nil {
		return TrustedUserCASet{}, err
	}
	set := TrustedUserCASet{Keys: make([]string, len(cas)), Principal: MachinePrincipal(request.UserMachineID), RevokedKeyIDs: revoked}
	for index, ca := range cas {
		set.Keys[index] = ca.PublicKey
	}
	return set, nil
}

// RotateUserCA advances one account's CA rotation by one step and returns its CAs: (1) add a pending CA hosts trust
// but nothing signs with; (2) once it is older than CAPropagation, it signs and the old one becomes retiring;
// (3) once the retiring one is older than the certificate validity, every certificate it signed has expired and it
// is removed. An operator calls it until only one active CA is left.
func (s *Service) RotateUserCA(ctx context.Context, accountID string, now time.Time) ([]UserCA, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(accountID) || now.IsZero() {
		return nil, ErrRotationInvalid
	}
	candidate, err := s.newUserCA()
	if err != nil {
		return nil, err
	}
	return s.repository.RotateUserCA(ctx, RotateUserCARequest{AccountID: accountID, Now: now.UTC(), Propagation: CAPropagation, Validity: s.certificates.Validity + time.Minute}, candidate)
}

func (s *Service) newUserCA() (NewUserCA, error) {
	public, private, err := ed25519.GenerateKey(s.random)
	if err != nil {
		return NewUserCA{}, err
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return NewUserCA{}, err
	}
	seed := private.Seed()
	sealed, err := secrets.EncryptBytes([]byte(s.certificates.EncryptionKey), seed)
	clear(seed)
	clear(private)
	if err != nil {
		return NewUserCA{}, err
	}
	id, err := uuid.NewRandomFromReader(s.random)
	if err != nil {
		return NewUserCA{}, err
	}
	return NewUserCA{ID: "sshca_" + id.String(), PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))), PrivateKeyCiphertext: sealed}, nil
}

func (s *Service) caSigner(ca UserCA) (ssh.Signer, error) {
	seed, err := secrets.DecryptBytes([]byte(s.certificates.EncryptionKey), ca.PrivateKeyCiphertext)
	if err != nil {
		return nil, err
	}
	defer clear(seed)
	if len(seed) != ed25519.SeedSize {
		return nil, secrets.ErrDecrypt
	}
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != ca.PublicKey {
		return nil, secrets.ErrDecrypt
	}
	return signer, nil
}
