package native

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/netip"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

const carrierDelegationKind = "paperboat.terminal-carrier/v1"
const carrierDelegationLifetime = 5 * time.Minute
const maximumCarrierOffer = 8 << 10

// CarrierClaims delegate only a terminal transport, never another machine or
// application grant. The parent binding still owns every revocation fence.
type CarrierClaims struct {
	Kind              string
	AccountID         string
	MachineID         string
	MachineGeneration uint64
	ParentPublicKey   string
	ExpiresUnix       int64
	Certificate       []byte
	Endpoints         []string
	DiscoPublicKey    string
	OwnerInstance     string
	RelayNodeIDs      []string
}

type CarrierOffer struct {
	Claims    CarrierClaims
	Signature []byte
}

// CarrierIdentity owns the new generation's private key. Only Offer travels
// over the old authenticated connection.
type CarrierIdentity struct {
	Offer CarrierOffer
	tls   *tls.Config
}

// PrepareCarrierIdentity creates the certificate before discovery starts, so
// its hash can isolate relay routes without inventing an endpoint placeholder.
func (o *Owner) PrepareCarrierIdentity() (*CarrierIdentity, error) {
	if o == nil || o.authority == nil || o.tls == nil {
		return nil, ErrInvalid
	}
	self, err := o.authority.Self()
	if err != nil || self.Role != "machine" || len(o.tls.Certificates) != 1 || len(o.tls.Certificates[0].Certificate) == 0 {
		return nil, ErrInvalid
	}
	parentKey, ok := o.tls.Certificates[0].PrivateKey.(ed25519.PrivateKey)
	if !ok || base64.RawURLEncoding.EncodeToString(parentKey.Public().(ed25519.PublicKey)) != self.QUICPublicKey {
		return nil, ErrInvalid
	}
	parentLeaf, err := x509.ParseCertificate(o.tls.Certificates[0].Certificate[0])
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expires := min(now.Add(carrierDelegationLifetime).Unix(), parentLeaf.NotAfter.Unix())
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: self.MachineID}, NotBefore: now.Add(-time.Minute), NotAfter: time.Unix(expires, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, err
	}
	claims := CarrierClaims{Kind: carrierDelegationKind, AccountID: self.AccountID, MachineID: self.MachineID, MachineGeneration: self.MachineGeneration, ParentPublicKey: self.QUICPublicKey, ExpiresUnix: expires, Certificate: der}
	offer := CarrierOffer{Claims: claims}

	config := o.tls.Clone()
	config.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}
	config.GetCertificate, config.GetClientCertificate = nil, nil
	config.NextProtos = []string{peerquic.ALPN}
	return &CarrierIdentity{Offer: offer, tls: config}, nil
}

func (i *CarrierIdentity) Namespace() [32]byte { return sha256.Sum256(i.Offer.Claims.Certificate) }

// SignCarrierCandidates binds STUN/local candidates and the generation's disco
// key to the same delegated certificate verified on the old authenticated link.
func (o *Owner) SignCarrierCandidates(identity *CarrierIdentity, instance, disco string, endpoints []netip.AddrPort, relays []string) error {
	if o == nil || identity == nil {
		return ErrInvalid
	}
	self, err := o.authority.Self()
	if err != nil {
		return err
	}
	parent, ok := o.tls.Certificates[0].PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return ErrInvalid
	}
	claims := identity.Offer.Claims
	claims.DiscoPublicKey, claims.OwnerInstance, claims.Endpoints = disco, instance, nil
	claims.RelayNodeIDs = append([]string(nil), relays...)
	for _, endpoint := range endpoints {
		claims.Endpoints = append(claims.Endpoints, endpoint.String())
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return err
	}
	offer := CarrierOffer{Claims: claims, Signature: ed25519.Sign(parent, body)}
	if err := offer.verify(self, time.Now()); err != nil {
		return err
	}
	identity.Offer = offer
	return nil
}

func (o CarrierOffer) MarshalBinary() ([]byte, error) {
	encoded, err := json.Marshal(o)
	if err != nil || len(encoded) > maximumCarrierOffer {
		return nil, ErrInvalid
	}
	return encoded, nil
}

func ParseCarrierOffer(encoded []byte) (CarrierOffer, error) {
	var offer CarrierOffer
	if len(encoded) == 0 || len(encoded) > maximumCarrierOffer {
		return offer, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&offer); err != nil {
		return CarrierOffer{}, ErrInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return CarrierOffer{}, ErrInvalid
	}
	return offer, nil
}

func (o CarrierOffer) verify(parent tailnet.NetworkBinding, now time.Time) error {
	c := o.Claims
	if parent.Role != "machine" || c.Kind != carrierDelegationKind || c.AccountID != parent.AccountID || c.MachineID != parent.MachineID || c.MachineGeneration != parent.MachineGeneration || c.ParentPublicKey != parent.QUICPublicKey || c.ExpiresUnix <= now.Unix() || c.ExpiresUnix > now.Add(carrierDelegationLifetime).Unix() || len(c.Certificate) > 4096 || len(c.Endpoints) == 0 || len(c.Endpoints) > tailnet.MaxPeerEndpoints {
		return ErrInvalid
	}
	if c.OwnerInstance != "" {
		raw, err := hex.DecodeString(c.OwnerInstance)
		if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != c.OwnerInstance {
			return ErrInvalid
		}
	}
	disco, err := derpquic.ParseDiscoKey(c.DiscoPublicKey)
	if err != nil || disco.IsZero() {
		return ErrInvalid
	}
	if len(c.RelayNodeIDs) > tailnet.MaxRegionalCandidates {
		return ErrInvalid
	}
	seenRelay := map[string]bool{}
	for _, id := range c.RelayNodeIDs {
		if id == "" || len(id) > 128 || seenRelay[id] {
			return ErrInvalid
		}
		seenRelay[id] = true
	}
	seen := map[netip.AddrPort]bool{}
	for _, raw := range c.Endpoints {
		endpoint, err := netip.ParseAddrPort(raw)
		if err != nil || endpoint.String() != raw || endpoint.Port() == 0 || endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() || endpoint.Addr().Zone() != "" || seen[endpoint] {
			return ErrInvalid
		}
		seen[endpoint] = true
	}
	public, err := base64.RawURLEncoding.Strict().DecodeString(parent.QUICPublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return ErrInvalid
	}
	body, err := json.Marshal(c)
	if err != nil || !ed25519.Verify(public, body, o.Signature) {
		return ErrInvalid
	}
	leaf, err := x509.ParseCertificate(c.Certificate)
	if err != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Unix() != c.ExpiresUnix {
		return ErrInvalid
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return ErrInvalid
	}
	if leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
		return ErrInvalid
	}
	return nil
}
