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
	"sync"
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
	// AllApplications says the generation serves every application, not only
	// terminals: a client that moved routes its new connections to it.
	AllApplications bool
}

type CarrierOffer struct {
	Claims    CarrierClaims
	Signature []byte
}

// CarrierIdentity owns the new generation's private key. Only Offer travels
// over the old authenticated connection. Offer is set before the generation
// starts; afterwards Current is the renewed offer (RenewCarrierIdentity).
type CarrierIdentity struct {
	Offer   CarrierOffer
	tls     *tls.Config
	private ed25519.PrivateKey
	mu      sync.Mutex
	leaf    *tls.Certificate
}

// PrepareCarrierIdentity creates the certificate before discovery starts, so
// its key can isolate relay routes without inventing an endpoint placeholder.
func (o *Owner) PrepareCarrierIdentity() (*CarrierIdentity, error) {
	if o == nil || o.authority == nil || o.tls == nil {
		return nil, ErrInvalid
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	self, der, expires, err := o.delegationCertificate(private)
	if err != nil {
		return nil, err
	}
	claims := CarrierClaims{Kind: carrierDelegationKind, AccountID: self.AccountID, MachineID: self.MachineID, MachineGeneration: self.MachineGeneration, ParentPublicKey: self.QUICPublicKey, ExpiresUnix: expires, Certificate: der}
	identity := &CarrierIdentity{Offer: CarrierOffer{Claims: claims}, private: private, leaf: &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}}
	config := o.tls.Clone()
	config.GetCertificate, config.GetClientCertificate = nil, nil
	config.NextProtos = []string{peerquic.ALPN}
	identity.tls = config
	return identity, nil
}

// serverTLS presents the current (renewed) delegated certificate. Each
// incoming flow takes its own copy, so renewal never touches an open one.
func (i *CarrierIdentity) serverTLS() *tls.Config {
	config := i.tls.Clone()
	i.mu.Lock()
	config.Certificates = []tls.Certificate{*i.leaf}
	i.mu.Unlock()
	return config
}

// delegationCertificate issues a self-signed certificate for private that
// expires with the delegation, never after the parent machine certificate.
func (o *Owner) delegationCertificate(private ed25519.PrivateKey) (tailnet.NetworkBinding, []byte, int64, error) {
	self, err := o.authority.Self()
	if err != nil || self.Role != "machine" || len(o.tls.Certificates) != 1 || len(o.tls.Certificates[0].Certificate) == 0 {
		return self, nil, 0, ErrInvalid
	}
	parentKey, ok := o.tls.Certificates[0].PrivateKey.(ed25519.PrivateKey)
	if !ok || base64.RawURLEncoding.EncodeToString(parentKey.Public().(ed25519.PublicKey)) != self.QUICPublicKey {
		return self, nil, 0, ErrInvalid
	}
	parent := o.tls.Certificates[0]
	// A long-lived generation renews past the first leaf; the parent issues fresh ones.
	if o.tls.GetCertificate != nil {
		if fresh, err := o.tls.GetCertificate(nil); err == nil && fresh != nil && len(fresh.Certificate) != 0 {
			parent = *fresh
		}
	}
	parentLeaf, err := x509.ParseCertificate(parent.Certificate[0])
	if err != nil {
		return self, nil, 0, err
	}
	now := time.Now().UTC()
	expires := min(now.Add(carrierDelegationLifetime).Unix(), parentLeaf.NotAfter.Unix())
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return self, nil, 0, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: self.MachineID}, NotBefore: now.Add(-time.Minute), NotAfter: time.Unix(expires, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, private.Public(), private)
	return self, der, expires, err
}

// Namespace keys the generation's network and relay routes. It follows the
// delegated key, not the certificate, so a renewed delegation keeps it.
func (i *CarrierIdentity) Namespace() [32]byte { return i.Current().Namespace() }

// Current is the generation's latest signed offer.
func (i *CarrierIdentity) Current() CarrierOffer {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.Offer
}

func (o CarrierOffer) Namespace() [32]byte { return certificateNamespace(o.Claims.Certificate) }

// certificateNamespace hashes the certificate's ed25519 key; an unparsable
// certificate (which never verifies) hashes as itself.
func certificateNamespace(der []byte) [32]byte {
	if leaf, err := x509.ParseCertificate(der); err == nil {
		if public, ok := leaf.PublicKey.(ed25519.PublicKey); ok {
			return sha256.Sum256(public)
		}
	}
	return sha256.Sum256(der)
}

// SignCarrierCandidates binds STUN/local candidates and the generation's disco
// key to the same delegated certificate verified on the old authenticated link.
func (o *Owner) SignCarrierCandidates(identity *CarrierIdentity, instance, disco string, endpoints []netip.AddrPort, relays []string) error {
	if o == nil || identity == nil {
		return ErrInvalid
	}
	claims := identity.Current().Claims
	claims.DiscoPublicKey, claims.OwnerInstance, claims.Endpoints = disco, instance, nil
	claims.RelayNodeIDs = append([]string(nil), relays...)
	for _, endpoint := range endpoints {
		claims.Endpoints = append(claims.Endpoints, endpoint.String())
	}
	_, err := o.signDelegation(identity, claims, nil)
	return err
}

// RenewCarrierIdentity extends a serving generation's delegation by another
// lifetime with the same key, signing the network's current candidates, so
// new clients keep reaching the same generation. Open connections never
// depended on the old offer's expiry.
func (o *Owner) RenewCarrierIdentity(identity *CarrierIdentity, network *tailnet.CarrierNetwork) (CarrierOffer, error) {
	if o == nil || identity == nil || network == nil {
		return CarrierOffer{}, ErrInvalid
	}
	_, der, expires, err := o.delegationCertificate(identity.private)
	if err != nil {
		return CarrierOffer{}, err
	}
	claims := identity.Current().Claims
	claims.ExpiresUnix, claims.Certificate, claims.DiscoPublicKey, claims.Endpoints = expires, der, network.DiscoPublic(), nil
	claims.RelayNodeIDs = network.RelayNodeIDs()
	for _, endpoint := range network.Endpoints() {
		claims.Endpoints = append(claims.Endpoints, endpoint.String())
	}
	return o.signDelegation(identity, claims, &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: identity.private})
}

func (o *Owner) signDelegation(identity *CarrierIdentity, claims CarrierClaims, leaf *tls.Certificate) (CarrierOffer, error) {
	self, err := o.authority.Self()
	if err != nil {
		return CarrierOffer{}, err
	}
	parent, ok := o.tls.Certificates[0].PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return CarrierOffer{}, ErrInvalid
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return CarrierOffer{}, err
	}
	offer := CarrierOffer{Claims: claims, Signature: ed25519.Sign(parent, body)}
	if err := offer.verify(self, time.Now()); err != nil {
		return CarrierOffer{}, err
	}
	identity.mu.Lock()
	identity.Offer = offer
	if leaf != nil {
		identity.leaf = leaf
	}
	identity.mu.Unlock()
	return offer, nil
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
