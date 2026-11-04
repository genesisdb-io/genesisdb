package core

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Event signatures prove which key committed an event. They complement the
// hash chain, which only proves that the log is consistent.
//
// A signature value has the form
//
//	genesisdb:signature:v1:<key id>:<signature>
//
// where <key id> is SigningKeyID of the public key and <signature> is the
// lowercase hex Ed25519 signature of the bytes
//
//	genesisdb:signature:v1:<event hash>
//
// with <event hash> taken as text exactly as it appears in the hash field.
// The signature covers the hash only. The hash binds metadata, payload,
// reference key and predecessor, so erasing a reference payload leaves both
// the hash and the signature valid.
const SignaturePrefix = "genesisdb:signature:v1:"

// ErrInvalidSigningKey reports a signing key that cannot be used.
var ErrInvalidSigningKey = errors.New("invalid signing key")

// EventSigner signs event hashes with one Ed25519 private key. It is safe
// for concurrent use.
type EventSigner struct {
	privateKey ed25519.PrivateKey
	publicKey  SigningPublicKey
}

// SigningPublicKey is a public key together with its ID.
type SigningPublicKey struct {
	// KeyID is the first 16 hex digits of SHA-256 of the raw public key.
	KeyID string `json:"keyId"`
	// PublicKey is the raw 32 byte Ed25519 public key, base64 encoded.
	PublicKey string `json:"publicKey"`
}

// NewEventSigner returns a signer for an Ed25519 private key.
func NewEventSigner(privateKey ed25519.PrivateKey) (*EventSigner, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: Ed25519 private key must have %d bytes", ErrInvalidSigningKey, ed25519.PrivateKeySize)
	}
	public := privateKey.Public().(ed25519.PublicKey)
	return &EventSigner{privateKey: privateKey, publicKey: NewSigningPublicKey(public)}, nil
}

// ParseSigningKey parses an Ed25519 private key in PKCS#8, PEM encoded or
// DER, as produced by "openssl genpkey -algorithm ED25519".
func ParseSigningKey(encoded []byte) (*EventSigner, error) {
	der := encoded
	if block, _ := pem.Decode(encoded); block != nil {
		if block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("%w: expected a PKCS#8 \"PRIVATE KEY\" PEM block, got %q", ErrInvalidSigningKey, block.Type)
		}
		der = block.Bytes
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSigningKey, err)
	}
	privateKey, isEd25519 := parsed.(ed25519.PrivateKey)
	if !isEd25519 {
		return nil, fmt.Errorf("%w: expected an Ed25519 key, got %T", ErrInvalidSigningKey, parsed)
	}
	return NewEventSigner(privateKey)
}

// NewSigningPublicKey returns the public key with its ID.
func NewSigningPublicKey(publicKey ed25519.PublicKey) SigningPublicKey {
	return SigningPublicKey{KeyID: SigningKeyID(publicKey), PublicKey: base64.StdEncoding.EncodeToString(publicKey)}
}

// ParseSigningPublicKey parses a public key in PKIX PEM or DER, or a raw
// 32 byte key in base64.
func ParseSigningPublicKey(encoded []byte) (SigningPublicKey, error) {
	text := strings.TrimSpace(string(encoded))
	if raw, err := base64.StdEncoding.DecodeString(text); err == nil && len(raw) == ed25519.PublicKeySize {
		return NewSigningPublicKey(raw), nil
	}
	der := encoded
	if block, _ := pem.Decode(encoded); block != nil {
		der = block.Bytes
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return SigningPublicKey{}, fmt.Errorf("%w: %v", ErrInvalidSigningKey, err)
	}
	publicKey, isEd25519 := parsed.(ed25519.PublicKey)
	if !isEd25519 {
		return SigningPublicKey{}, fmt.Errorf("%w: expected an Ed25519 public key, got %T", ErrInvalidSigningKey, parsed)
	}
	return NewSigningPublicKey(publicKey), nil
}

// SigningKeyID returns the ID of a public key.
func SigningKeyID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:8])
}

// PublicKey returns the public key of the signer.
func (signer *EventSigner) PublicKey() SigningPublicKey { return signer.publicKey }

// SignHash returns the signature value of an event hash.
func (signer *EventSigner) SignHash(hash string) string {
	signature := ed25519.Sign(signer.privateKey, signedMessage(hash))
	return SignaturePrefix + signer.publicKey.KeyID + ":" + hex.EncodeToString(signature)
}

func signedMessage(hash string) []byte {
	return []byte(SignaturePrefix + hash)
}

// SignatureKeyID returns the key ID of a signature value, or false if the
// value is not a signature of a known version.
func SignatureKeyID(signature string) (string, bool) {
	keyID, _, isValid := splitSignature(signature)
	return keyID, isValid
}

func splitSignature(signature string) (string, []byte, bool) {
	rest, hasPrefix := strings.CutPrefix(signature, SignaturePrefix)
	if !hasPrefix {
		return "", nil, false
	}
	keyID, encoded, hasSeparator := strings.Cut(rest, ":")
	if !hasSeparator || len(keyID) != 16 {
		return "", nil, false
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return "", nil, false
	}
	return keyID, raw, true
}

// SigningKeySet holds trusted public keys by key ID.
type SigningKeySet map[string]ed25519.PublicKey

// NewSigningKeySet decodes public keys. Keys whose ID does not match their
// content are rejected.
func NewSigningKeySet(keys []SigningPublicKey) (SigningKeySet, error) {
	set := SigningKeySet{}
	for _, key := range keys {
		raw, err := base64.StdEncoding.DecodeString(key.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: public key %s is not a base64 Ed25519 key", ErrInvalidSigningKey, key.KeyID)
		}
		if SigningKeyID(raw) != key.KeyID {
			return nil, fmt.Errorf("%w: public key %s does not match its ID", ErrInvalidSigningKey, key.KeyID)
		}
		set[key.KeyID] = raw
	}
	return set, nil
}

// SignatureStatus is the result of checking one event signature.
type SignatureStatus int

const (
	// SignatureAbsent: the event carries no signature.
	SignatureAbsent SignatureStatus = iota
	// SignatureValid: the signature verifies with a key of the set.
	SignatureValid
	// SignatureUnknownKey: the key ID is not in the set, nothing is proven.
	SignatureUnknownKey
	// SignatureInvalid: malformed, or does not verify with its key.
	SignatureInvalid
)

// VerifySignature checks the signature of an event against its hash.
func (set SigningKeySet) VerifySignature(event *Event) SignatureStatus {
	if event.Signature == "" {
		return SignatureAbsent
	}
	keyID, raw, isWellFormed := splitSignature(event.Signature)
	if !isWellFormed {
		return SignatureInvalid
	}
	publicKey, isKnown := set[keyID]
	if !isKnown {
		return SignatureUnknownKey
	}
	if !ed25519.Verify(publicKey, signedMessage(event.Hash), raw) {
		return SignatureInvalid
	}
	return SignatureValid
}
