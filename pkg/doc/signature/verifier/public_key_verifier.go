/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcec"
	gojose "github.com/square/go-jose/v3"

	"github.com/hyperledger/aries-framework-go/pkg/doc/bbs/bbs12381g2pub"
	"github.com/hyperledger/aries-framework-go/pkg/doc/jose"
)

// PublicKeyVerifier makes signature verification using the public key
// based on one or several signature algorithms.
type PublicKeyVerifier struct {
	exactType      string
	singleVerifier SignatureVerifier
	verifiers      []SignatureVerifier
}

// PublicKeyVerifierOpt is the PublicKeyVerifier functional option.
type PublicKeyVerifierOpt func(opts *PublicKeyVerifier)

// NewPublicKeyVerifier creates a new PublicKeyVerifier based on single signature algorithm.
func NewPublicKeyVerifier(sigAlg SignatureVerifier, opts ...PublicKeyVerifierOpt) *PublicKeyVerifier {
	v := &PublicKeyVerifier{
		singleVerifier: sigAlg,
	}

	for _, opt := range opts {
		opt(v)
	}

	return v
}

// NewCompositePublicKeyVerifier creates a new PublicKeyVerifier based on one or more signature algorithms.
func NewCompositePublicKeyVerifier(verifiers []SignatureVerifier, opts ...PublicKeyVerifierOpt) *PublicKeyVerifier {
	v := &PublicKeyVerifier{
		verifiers: verifiers,
	}

	for _, opt := range opts {
		opt(v)
	}

	return v
}

// Verify verifies the signature.
func (pkv *PublicKeyVerifier) Verify(pubKey *PublicKey, msg, signature []byte) error {
	if pkv.exactType != "" {
		if pubKey.Type != pkv.exactType {
			return fmt.Errorf("a type of public key is not '%s'", pkv.exactType)
		}
	}

	if pkv.singleVerifier != nil {
		if pubKey.JWK != nil && !pkv.matchVerifier(pkv.singleVerifier, pubKey.JWK) {
			return errors.New("verifier does not match JSON Web Key")
		}

		return pkv.singleVerifier.Verify(pubKey, msg, signature)
	}

	for _, v := range pkv.verifiers {
		if pkv.matchVerifier(v, pubKey.JWK) {
			return v.Verify(pubKey, msg, signature)
		}
	}

	return errors.New("no matching verifier found")
}

func (pkv *PublicKeyVerifier) matchVerifier(verifier SignatureVerifier, jwk *jose.JWK) bool {
	// "kty" is a mandatory field in JWK.
	if verifier.KeyType() != jwk.Kty {
		return false
	}

	// "crv" is an optional field in JWK (however, it's mandatory for elliptic curves).
	if jwk.Crv != "" && verifier.Curve() != jwk.Crv {
		return false
	}

	// "alg" is an optional field in JWK.
	if jwk.Algorithm != "" && verifier.Algorithm() != jwk.Algorithm {
		return false
	}

	return true
}

// WithExactPublicKeyType option is used to check the type of the PublicKey.
func WithExactPublicKeyType(jwkType string) PublicKeyVerifierOpt {
	return func(opts *PublicKeyVerifier) {
		opts.exactType = jwkType
	}
}

// SignatureVerifier make signature verification of a certain algorithm (e.g. Ed25519 or ECDSA secp256k1).
type SignatureVerifier interface {
	KeyType() string

	Curve() string

	Algorithm() string

	Verify(pubKey *PublicKey, msg, signature []byte) error
}

type baseSignatureVerifier struct {
	keyType   string
	curve     string
	algorithm string
}

func (sv baseSignatureVerifier) KeyType() string {
	return sv.keyType
}

func (sv baseSignatureVerifier) Curve() string {
	return sv.curve
}

func (sv baseSignatureVerifier) Algorithm() string {
	return sv.algorithm
}

// Ed25519SignatureVerifier verifies a Ed25519 signature taking Ed25519 public key bytes as input.
type Ed25519SignatureVerifier struct {
	baseSignatureVerifier
}

// NewEd25519SignatureVerifier creates a new Ed25519SignatureVerifier.
func NewEd25519SignatureVerifier() *Ed25519SignatureVerifier {
	return &Ed25519SignatureVerifier{
		baseSignatureVerifier: baseSignatureVerifier{
			keyType:   "OKP",
			curve:     "Ed25519",
			algorithm: "EdDSA",
		},
	}
}

// Verify verifies the signature.
func (sv Ed25519SignatureVerifier) Verify(pubKey *PublicKey, msg, signature []byte) error {
	value := pubKey.Value

	if pubKey.JWK != nil {
		var ok bool
		value, ok = pubKey.JWK.Public().Key.(ed25519.PublicKey)

		if !ok {
			return fmt.Errorf("public key not ed25519.PublicKey")
		}
	}
	// ed25519 panics if key size is wrong
	if len(value) != ed25519.PublicKeySize {
		return errors.New("ed25519: invalid key")
	}

	verified := ed25519.Verify(value, msg, signature)
	if !verified {
		return errors.New("ed25519: invalid signature")
	}

	return nil
}

// RSAPS256SignatureVerifier verifies a Ed25519 signature taking RSA public key bytes as input.
type RSAPS256SignatureVerifier struct {
	baseSignatureVerifier
}

// NewRSAPS256SignatureVerifier creates a new RSAPS256SignatureVerifier.
func NewRSAPS256SignatureVerifier() *RSAPS256SignatureVerifier {
	return &RSAPS256SignatureVerifier{
		baseSignatureVerifier: baseSignatureVerifier{
			keyType:   "RSA",
			algorithm: "PS256",
		},
	}
}

// Verify verifies the signature.
func (sv RSAPS256SignatureVerifier) Verify(key *PublicKey, msg, signature []byte) error {
	pubKey, err := x509.ParsePKCS1PublicKey(key.Value)
	if err != nil {
		return errors.New("rsa: invalid public key")
	}

	hash := crypto.SHA256
	hasher := hash.New()

	_, err = hasher.Write(msg)
	if err != nil {
		return errors.New("rsa: hash error")
	}

	hashed := hasher.Sum(nil)

	err = rsa.VerifyPSS(pubKey, hash, hashed, signature, nil)
	if err != nil {
		return errors.New("rsa: invalid signature")
	}

	return nil
}

const (
	p256KeySize      = 32
	p384KeySize      = 48
	p521KeySize      = 66
	secp256k1KeySize = 32
)

// ECDSASignatureVerifier verifies elliptic curve signatures.
type ECDSASignatureVerifier struct {
	baseSignatureVerifier

	ec ellipticCurve
}

// Verify verifies the signature.
func (sv *ECDSASignatureVerifier) Verify(pubKey *PublicKey, msg, signature []byte) error {
	pubKeyJWK := pubKey.JWK
	if pubKeyJWK == nil {
		jwk, err := sv.createJWK(pubKey.Value)
		if err != nil {
			return fmt.Errorf("ecdsa: create JWK from public key bytes: %w", err)
		}

		pubKeyJWK = jwk
	}

	ec := sv.ec

	ecdsaPubKey, ok := pubKeyJWK.Key.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("ecdsa: invalid public key type")
	}

	if len(signature) != 2*ec.keySize {
		return errors.New("ecdsa: invalid signature size")
	}

	hasher := ec.hash.New()

	_, err := hasher.Write(msg)
	if err != nil {
		return errors.New("ecdsa: hash error")
	}

	hash := hasher.Sum(nil)

	r := big.NewInt(0).SetBytes(signature[:ec.keySize])
	s := big.NewInt(0).SetBytes(signature[ec.keySize:])

	verified := ecdsa.Verify(ecdsaPubKey, hash, r, s)
	if !verified {
		return errors.New("ecdsa: invalid signature")
	}

	return nil
}

func (sv *ECDSASignatureVerifier) createJWK(pubKeyBytes []byte) (*jose.JWK, error) {
	curve := sv.ec.curve

	x, y := elliptic.Unmarshal(curve, pubKeyBytes)
	if x == nil {
		return nil, errors.New("invalid public key")
	}

	ecdsaPubKey := &ecdsa.PublicKey{
		Curve: curve,
		X:     x,
		Y:     y,
	}

	return &jose.JWK{
		JSONWebKey: gojose.JSONWebKey{
			Key:       ecdsaPubKey,
			Algorithm: sv.algorithm,
		},
		Kty: sv.keyType,
		Crv: sv.curve,
	}, nil
}

// NewECDSASecp256k1SignatureVerifier creates a new signature verifier that verifies a ECDSA secp256k1 signature
// taking public key bytes and JSON Web Key as input.
func NewECDSASecp256k1SignatureVerifier() *ECDSASignatureVerifier {
	return &ECDSASignatureVerifier{
		baseSignatureVerifier: baseSignatureVerifier{
			keyType:   "EC",
			curve:     "secp256k1",
			algorithm: "ES256K",
		},
		ec: ellipticCurve{
			curve:   btcec.S256(),
			keySize: secp256k1KeySize,
			hash:    crypto.SHA256,
		},
	}
}

// NewECDSAES256SignatureVerifier creates a new signature verifier that verifies a ECDSA P-256 signature
// taking public key bytes and JSON Web Key as input.
func NewECDSAES256SignatureVerifier() *ECDSASignatureVerifier {
	return &ECDSASignatureVerifier{
		baseSignatureVerifier: baseSignatureVerifier{
			keyType:   "EC",
			curve:     "P-256",
			algorithm: "ES256",
		},
		ec: ellipticCurve{
			curve:   elliptic.P256(),
			keySize: p256KeySize,
			hash:    crypto.SHA256,
		},
	}
}

// NewECDSAES384SignatureVerifier creates a new signature verifier that verifies a ECDSA P-384 signature
// taking public key bytes and JSON Web Key as input.
func NewECDSAES384SignatureVerifier() *ECDSASignatureVerifier {
	return &ECDSASignatureVerifier{
		baseSignatureVerifier: baseSignatureVerifier{
			keyType:   "EC",
			curve:     "P-384",
			algorithm: "ES384",
		},
		ec: ellipticCurve{
			curve:   elliptic.P384(),
			keySize: p384KeySize,
			hash:    crypto.SHA384,
		},
	}
}

// NewECDSAES521SignatureVerifier creates a new signature verifier that verifies a ECDSA P-521 signature
// taking public key bytes and JSON Web Key as input.
func NewECDSAES521SignatureVerifier() *ECDSASignatureVerifier {
	return &ECDSASignatureVerifier{
		baseSignatureVerifier: baseSignatureVerifier{
			keyType:   "EC",
			curve:     "P-521",
			algorithm: "ES521",
		},
		ec: ellipticCurve{
			curve:   elliptic.P521(),
			keySize: p521KeySize,
			hash:    crypto.SHA512,
		},
	}
}

// NewBBSG2SignatureVerifier creates a new BBSG2SignatureVerifier.
func NewBBSG2SignatureVerifier() *BBSG2SignatureVerifier {
	return &BBSG2SignatureVerifier{}
}

// BBSG2SignatureVerifier is a signature verifier that verifies a BBS+ Signature
// taking Bls12381G2Key2020 public key bytes as input.
// The reference implementation https://github.com/mattrglobal/bls12381-key-pair supports public key bytes only,
// JWK is not supported.
type BBSG2SignatureVerifier struct {
	baseSignatureVerifier
}

// Verify verifies the signature.
func (v *BBSG2SignatureVerifier) Verify(pubKeyValue *PublicKey, doc, signature []byte) error {
	bbs := bbs12381g2pub.New()

	return bbs.Verify(v.splitMessageIntoLines(string(doc)), signature, pubKeyValue.Value)
}

func (v *BBSG2SignatureVerifier) splitMessageIntoLines(msg string) [][]byte {
	rows := strings.Split(msg, "\n")

	msgs := make([][]byte, 0, len(rows))

	for i := range rows {
		if strings.TrimSpace(rows[i]) != "" {
			msgs = append(msgs, []byte(rows[i]))
		}
	}

	return msgs
}

type ellipticCurve struct {
	curve   elliptic.Curve
	keySize int
	hash    crypto.Hash
}
