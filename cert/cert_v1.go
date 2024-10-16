package cert

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/crypto/curve25519"
	"google.golang.org/protobuf/proto"
)

const publicKeyLen = 32

type certificateV1 struct {
	details   detailsV1
	signature []byte
}

type detailsV1 struct {
	name           string
	networks       []netip.Prefix
	unsafeNetworks []netip.Prefix
	groups         []string
	notBefore      time.Time
	notAfter       time.Time
	publicKey      []byte
	isCA           bool
	issuer         string

	curve Curve
}

type m map[string]interface{}

func (c *certificateV1) Version() Version {
	return Version1
}

func (c *certificateV1) Curve() Curve {
	return c.details.curve
}

func (c *certificateV1) Groups() []string {
	return c.details.groups
}

func (c *certificateV1) IsCA() bool {
	return c.details.isCA
}

func (c *certificateV1) Issuer() string {
	return c.details.issuer
}

func (c *certificateV1) Name() string {
	return c.details.name
}

func (c *certificateV1) Networks() []netip.Prefix {
	return c.details.networks
}

func (c *certificateV1) NotAfter() time.Time {
	return c.details.notAfter
}

func (c *certificateV1) NotBefore() time.Time {
	return c.details.notBefore
}

func (c *certificateV1) PublicKey() []byte {
	return c.details.publicKey
}

func (c *certificateV1) Signature() []byte {
	return c.signature
}

func (c *certificateV1) UnsafeNetworks() []netip.Prefix {
	return c.details.unsafeNetworks
}

func (c *certificateV1) Fingerprint() (string, error) {
	b, err := c.Marshal()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (c *certificateV1) CheckSignature(key []byte) bool {
	b, err := proto.Marshal(c.getRawDetails())
	if err != nil {
		return false
	}
	switch c.details.curve {
	case Curve_CURVE25519:
		return ed25519.Verify(key, b, c.signature)
	case Curve_P256:
		x, y := elliptic.Unmarshal(elliptic.P256(), key)
		pubKey := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		hashed := sha256.Sum256(b)
		return ecdsa.VerifyASN1(pubKey, hashed[:], c.signature)
	default:
		return false
	}
}

func (c *certificateV1) Expired(t time.Time) bool {
	return c.details.notBefore.After(t) || c.details.notAfter.Before(t)
}

func (c *certificateV1) VerifyPrivateKey(curve Curve, key []byte) error {
	if curve != c.details.curve {
		return fmt.Errorf("curve in cert and private key supplied don't match")
	}
	if c.details.isCA {
		switch curve {
		case Curve_CURVE25519:
			// the call to PublicKey below will panic slice bounds out of range otherwise
			if len(key) != ed25519.PrivateKeySize {
				return fmt.Errorf("key was not 64 bytes, is invalid ed25519 private key")
			}

			if !ed25519.PublicKey(c.details.publicKey).Equal(ed25519.PrivateKey(key).Public()) {
				return fmt.Errorf("public key in cert and private key supplied don't match")
			}
		case Curve_P256:
			privkey, err := ecdh.P256().NewPrivateKey(key)
			if err != nil {
				return fmt.Errorf("cannot parse private key as P256: %w", err)
			}
			pub := privkey.PublicKey().Bytes()
			if !bytes.Equal(pub, c.details.publicKey) {
				return fmt.Errorf("public key in cert and private key supplied don't match")
			}
		default:
			return fmt.Errorf("invalid curve: %s", curve)
		}
		return nil
	}

	var pub []byte
	switch curve {
	case Curve_CURVE25519:
		var err error
		pub, err = curve25519.X25519(key, curve25519.Basepoint)
		if err != nil {
			return err
		}
	case Curve_P256:
		privkey, err := ecdh.P256().NewPrivateKey(key)
		if err != nil {
			return err
		}
		pub = privkey.PublicKey().Bytes()
	default:
		return fmt.Errorf("invalid curve: %s", curve)
	}
	if !bytes.Equal(pub, c.details.publicKey) {
		return fmt.Errorf("public key in cert and private key supplied don't match")
	}

	return nil
}

// getRawDetails marshals the raw details into protobuf ready struct
func (c *certificateV1) getRawDetails() *RawNebulaCertificateDetails {
	rd := &RawNebulaCertificateDetails{
		Name:      c.details.name,
		Groups:    c.details.groups,
		NotBefore: c.details.notBefore.Unix(),
		NotAfter:  c.details.notAfter.Unix(),
		PublicKey: make([]byte, len(c.details.publicKey)),
		IsCA:      c.details.isCA,
		Curve:     c.details.curve,
	}

	for _, ipNet := range c.details.networks {
		mask := net.CIDRMask(ipNet.Bits(), ipNet.Addr().BitLen())
		rd.Ips = append(rd.Ips, addr2int(ipNet.Addr()), ip2int(mask))
	}

	for _, ipNet := range c.details.unsafeNetworks {
		mask := net.CIDRMask(ipNet.Bits(), ipNet.Addr().BitLen())
		rd.Subnets = append(rd.Subnets, addr2int(ipNet.Addr()), ip2int(mask))
	}

	copy(rd.PublicKey, c.details.publicKey[:])

	// I know, this is terrible
	rd.Issuer, _ = hex.DecodeString(c.details.issuer)

	return rd
}

func (c *certificateV1) String() string {
	b, err := json.MarshalIndent(c.marshalJSON(), "", "\t")
	if err != nil {
		return "<error marshalling certificate>"
	}
	return string(b)
}

func (c *certificateV1) MarshalForHandshakes() ([]byte, error) {
	pubKey := c.details.publicKey
	c.details.publicKey = nil
	rawCertNoKey, err := c.Marshal()
	if err != nil {
		return nil, err
	}
	c.details.publicKey = pubKey
	return rawCertNoKey, nil
}

func (c *certificateV1) Marshal() ([]byte, error) {
	rc := RawNebulaCertificate{
		Details:   c.getRawDetails(),
		Signature: c.signature,
	}

	return proto.Marshal(&rc)
}

func (c *certificateV1) MarshalPEM() ([]byte, error) {
	b, err := c.Marshal()
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: CertificateBanner, Bytes: b}), nil
}

func (c *certificateV1) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.marshalJSON())
}

func (c *certificateV1) marshalJSON() m {
	fp, _ := c.Fingerprint()
	return m{
		"version": Version1,
		"details": m{
			"name":           c.details.name,
			"networks":       c.details.networks,
			"unsafeNetworks": c.details.unsafeNetworks,
			"groups":         c.details.groups,
			"notBefore":      c.details.notBefore,
			"notAfter":       c.details.notAfter,
			"publicKey":      fmt.Sprintf("%x", c.details.publicKey),
			"isCa":           c.details.isCA,
			"issuer":         c.details.issuer,
			"curve":          c.details.curve.String(),
		},
		"fingerprint": fp,
		"signature":   fmt.Sprintf("%x", c.Signature()),
	}
}

func (c *certificateV1) Copy() Certificate {
	nc := &certificateV1{
		details: detailsV1{
			name:           c.details.name,
			groups:         make([]string, len(c.details.groups)),
			networks:       make([]netip.Prefix, len(c.details.networks)),
			unsafeNetworks: make([]netip.Prefix, len(c.details.unsafeNetworks)),
			notBefore:      c.details.notBefore,
			notAfter:       c.details.notAfter,
			publicKey:      make([]byte, len(c.details.publicKey)),
			isCA:           c.details.isCA,
			issuer:         c.details.issuer,
			curve:          c.details.curve,
		},
		signature: make([]byte, len(c.signature)),
	}

	copy(nc.signature, c.signature)
	copy(nc.details.groups, c.details.groups)
	copy(nc.details.publicKey, c.details.publicKey)
	copy(nc.details.networks, c.details.networks)
	copy(nc.details.unsafeNetworks, c.details.unsafeNetworks)

	return nc
}

func (c *certificateV1) fromTBSCertificate(t *TBSCertificate) error {
	c.details = detailsV1{
		name:           t.Name,
		networks:       t.Networks,
		unsafeNetworks: t.UnsafeNetworks,
		groups:         t.Groups,
		notBefore:      t.NotBefore,
		notAfter:       t.NotAfter,
		publicKey:      t.PublicKey,
		isCA:           t.IsCA,
		curve:          t.Curve,
		issuer:         t.issuer,
	}

	return nil
}

func (c *certificateV1) marshalForSigning() ([]byte, error) {
	b, err := proto.Marshal(c.getRawDetails())
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (c *certificateV1) setSignature(b []byte) error {
	c.signature = b
	return nil
}

// unmarshalCertificateV1 will unmarshal a protobuf byte representation of a nebula cert
// if the publicKey is provided here then it is not required to be present in `b`
func unmarshalCertificateV1(b []byte, publicKey []byte) (*certificateV1, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("nil byte array")
	}
	var rc RawNebulaCertificate
	err := proto.Unmarshal(b, &rc)
	if err != nil {
		return nil, err
	}

	if rc.Details == nil {
		return nil, fmt.Errorf("encoded Details was nil")
	}

	if len(rc.Details.Ips)%2 != 0 {
		return nil, fmt.Errorf("encoded IPs should be in pairs, an odd number was found")
	}

	if len(rc.Details.Subnets)%2 != 0 {
		return nil, fmt.Errorf("encoded Subnets should be in pairs, an odd number was found")
	}

	nc := certificateV1{
		details: detailsV1{
			name:           rc.Details.Name,
			groups:         make([]string, len(rc.Details.Groups)),
			networks:       make([]netip.Prefix, len(rc.Details.Ips)/2),
			unsafeNetworks: make([]netip.Prefix, len(rc.Details.Subnets)/2),
			notBefore:      time.Unix(rc.Details.NotBefore, 0),
			notAfter:       time.Unix(rc.Details.NotAfter, 0),
			publicKey:      make([]byte, len(rc.Details.PublicKey)),
			isCA:           rc.Details.IsCA,
			curve:          rc.Details.Curve,
		},
		signature: make([]byte, len(rc.Signature)),
	}

	copy(nc.signature, rc.Signature)
	copy(nc.details.groups, rc.Details.Groups)
	nc.details.issuer = hex.EncodeToString(rc.Details.Issuer)

	if len(publicKey) > 0 {
		nc.details.publicKey = publicKey
	}

	copy(nc.details.publicKey, rc.Details.PublicKey)

	var ip netip.Addr
	for i, rawIp := range rc.Details.Ips {
		if i%2 == 0 {
			ip = int2addr(rawIp)
		} else {
			ones, _ := net.IPMask(int2ip(rawIp)).Size()
			nc.details.networks[i/2] = netip.PrefixFrom(ip, ones)
		}
	}

	for i, rawIp := range rc.Details.Subnets {
		if i%2 == 0 {
			ip = int2addr(rawIp)
		} else {
			ones, _ := net.IPMask(int2ip(rawIp)).Size()
			nc.details.unsafeNetworks[i/2] = netip.PrefixFrom(ip, ones)
		}
	}

	//do not sort the subnets field for V1 certs

	return &nc, nil
}

func ip2int(ip []byte) uint32 {
	if len(ip) == 16 {
		return binary.BigEndian.Uint32(ip[12:16])
	}
	return binary.BigEndian.Uint32(ip)
}

func int2ip(nn uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, nn)
	return ip
}

func addr2int(addr netip.Addr) uint32 {
	b := addr.Unmap().As4()
	return binary.BigEndian.Uint32(b[:])
}

func int2addr(nn uint32) netip.Addr {
	ip := [4]byte{}
	binary.BigEndian.PutUint32(ip[:], nn)
	return netip.AddrFrom4(ip).Unmap()
}
