package cert

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"net/netip"
	"slices"
	"time"

	"github.com/slackhq/nebula/pkclient"
)

// TBSCertificate represents a certificate intended to be signed.
// It is invalid to use this structure as a Certificate.
type TBSCertificate struct {
	Version        Version
	Name           string
	Networks       []netip.Prefix
	UnsafeNetworks []netip.Prefix
	Groups         []string
	IsCA           bool
	NotBefore      time.Time
	NotAfter       time.Time
	PublicKey      []byte
	Curve          Curve
	issuer         string
}

type beingSignedCertificate interface {
	// fromTBSCertificate copies the values from the TBSCertificate to this versions internal representation
	fromTBSCertificate(*TBSCertificate) error

	// marshalForSigning returns the bytes that should be signed
	marshalForSigning() ([]byte, error)

	// setSignature sets the signature for the certificate that has just been signed
	setSignature([]byte) error
}

type SignerPredicate func(certBytes []byte) ([]byte, error)

// Sign will create a sealed certificate using details provided by the TBSCertificate as long as those
// details do not violate constraints of the signing certificate.
// If the TBSCertificate is a CA then signer must be nil.
func (t *TBSCertificate) Sign(signWith Certificate, curve Curve, key []byte) (Certificate, error) {
	switch t.Curve {
	case Curve_CURVE25519:
		signer := ed25519.PrivateKey(key)
		sp := func(certBytes []byte) ([]byte, error) {
			sig := ed25519.Sign(signer, certBytes)
			return sig, nil
		}
		return t.SignWithPredicate(signWith, curve, sp)
	case Curve_P256:
		signer := &ecdsa.PrivateKey{
			PublicKey: ecdsa.PublicKey{
				Curve: elliptic.P256(),
			},
			// ref: https://github.com/golang/go/blob/go1.19/src/crypto/x509/sec1.go#L95
			D: new(big.Int).SetBytes(key),
		}
		// ref: https://github.com/golang/go/blob/go1.19/src/crypto/x509/sec1.go#L119
		signer.X, signer.Y = signer.Curve.ScalarBaseMult(key)
		sp := func(certBytes []byte) ([]byte, error) {
			// We need to hash first for ECDSA
			// - https://pkg.go.dev/crypto/ecdsa#SignASN1
			hashed := sha256.Sum256(certBytes)
			return ecdsa.SignASN1(rand.Reader, signer, hashed[:])
		}
		return t.SignWithPredicate(signWith, curve, sp)
	default:
		return nil, fmt.Errorf("invalid curve: %s", t.Curve)
	}
}

func (t *TBSCertificate) SignPkcs11(signer Certificate, curve Curve, client *pkclient.PKClient) (Certificate, error) {
	if client == nil {
		return nil, fmt.Errorf("pkclient must be non-nil")
	}
	switch t.Curve {
	case Curve_CURVE25519:
		return nil, fmt.Errorf("only P256 is supported by PKCS#11")
	case Curve_P256:
		//todo: verify that pkcs11 hashes for you
		return t.SignWithPredicate(signer, curve, client.SignASN1)
	default:
		return nil, fmt.Errorf("invalid curve: %s", t.Curve)
	}
}

func (t *TBSCertificate) SignWithPredicate(signer Certificate, curve Curve, sp SignerPredicate) (Certificate, error) {
	if curve != t.Curve {
		return nil, fmt.Errorf("curve in cert and private key supplied don't match")
	}

	//TODO: make sure we have all minimum properties to sign, like a public key
	//TODO: we need to verify networks and unsafe networks (no duplicates, max of 1 of each version for v2 certs

	if signer != nil {
		if t.IsCA {
			return nil, fmt.Errorf("can not sign a CA certificate with another")
		}

		err := checkCAConstraints(signer, t.NotBefore, t.NotAfter, t.Groups, t.Networks, t.UnsafeNetworks)
		if err != nil {
			return nil, err
		}

		issuer, err := signer.Fingerprint()
		if err != nil {
			return nil, fmt.Errorf("error computing issuer: %v", err)
		}
		t.issuer = issuer
	} else {
		if !t.IsCA {
			return nil, fmt.Errorf("self signed certificates must have IsCA set to true")
		}
	}

	slices.SortFunc(t.Networks, comparePrefix)
	slices.SortFunc(t.UnsafeNetworks, comparePrefix)

	var c beingSignedCertificate
	switch t.Version {
	case Version1:
		c = &certificateV1{}
		err := c.fromTBSCertificate(t)
		if err != nil {
			return nil, err
		}
	case Version2:
		c = &certificateV2{}
		err := c.fromTBSCertificate(t)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown cert version %d", t.Version)
	}

	certBytes, err := c.marshalForSigning()
	if err != nil {
		return nil, err
	}

	sig, err := sp(certBytes)
	if err != nil {
		return nil, err
	}

	//TODO: check if we have sig bytes?
	err = c.setSignature(sig)
	if err != nil {
		return nil, err
	}

	sc, ok := c.(Certificate)
	if !ok {
		return nil, fmt.Errorf("invalid certificate")
	}

	return sc, nil
}

func comparePrefix(a, b netip.Prefix) int {
	addr := a.Addr().Compare(b.Addr())
	if addr == 0 {
		return a.Bits() - b.Bits()
	}
	return addr
}
