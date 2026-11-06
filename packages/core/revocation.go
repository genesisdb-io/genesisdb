package core

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// SigningKeyRevocation withdraws trust from a signing key, for example after
// the private key leaked. A leaked key can sign events that never were
// committed, so its signatures cannot prove anything from the point where
// the key may have been in other hands.
//
// With ValidThroughHash set, signatures of the key on events up to and
// including the event with that hash stay trusted, later ones are revoked.
// The hash also serves as a checkpoint: if no event of the log has it, the
// history before the revocation point was changed. Without ValidThroughHash
// every signature of the key is revoked.
type SigningKeyRevocation struct {
	KeyID            string `json:"keyId"`
	ValidThroughHash string `json:"validThroughHash,omitempty"`
}

// String returns the revocation in the form ParseSigningKeyRevocation reads.
func (revocation SigningKeyRevocation) String() string {
	if revocation.ValidThroughHash == "" {
		return revocation.KeyID
	}
	return revocation.KeyID + ":" + revocation.ValidThroughHash
}

// ParseSigningKeyRevocation reads "<key id>" or "<key id>:<event hash>".
func ParseSigningKeyRevocation(text string) (SigningKeyRevocation, error) {
	keyID, hash, hasPoint := strings.Cut(strings.TrimSpace(text), ":")
	revocation := SigningKeyRevocation{KeyID: strings.ToLower(keyID), ValidThroughHash: strings.ToLower(hash)}
	if hasPoint && hash == "" {
		return revocation, fmt.Errorf("%w: revocation of key %s names an empty event hash", ErrInvalidSigningKey, revocation.KeyID)
	}
	return revocation, revocation.validate()
}

func (revocation SigningKeyRevocation) validate() error {
	if !isLowercaseHex(revocation.KeyID, 16) {
		return fmt.Errorf("%w: revoked key ID %q must have 16 hex digits", ErrInvalidSigningKey, revocation.KeyID)
	}
	if revocation.ValidThroughHash != "" && !isLowercaseHex(revocation.ValidThroughHash, 64) {
		return fmt.Errorf("%w: revocation point of key %s must be an event hash with 64 hex digits", ErrInvalidSigningKey, revocation.KeyID)
	}
	return nil
}

func isLowercaseHex(text string, length int) bool {
	if len(text) != length || strings.ToLower(text) != text {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

// SigningKeyRevocations maps revoked key IDs to the hash of the last event
// their signatures stay trusted for, empty when every signature is revoked.
type SigningKeyRevocations map[string]string

// NewSigningKeyRevocations validates revocations. A key may be revoked once.
func NewSigningKeyRevocations(revocations []SigningKeyRevocation) (SigningKeyRevocations, error) {
	set := SigningKeyRevocations{}
	for _, revocation := range revocations {
		if err := revocation.validate(); err != nil {
			return nil, err
		}
		if _, isDuplicate := set[revocation.KeyID]; isDuplicate {
			return nil, fmt.Errorf("%w: key %s is revoked twice", ErrInvalidSigningKey, revocation.KeyID)
		}
		set[revocation.KeyID] = revocation.ValidThroughHash
	}
	return set, nil
}

// IsRevoked reports whether the key is revoked, at least partly.
func (set SigningKeyRevocations) IsRevoked(keyID string) bool {
	_, isRevoked := set[keyID]
	return isRevoked
}

// RevocationTracker follows a log in order and decides for each signature
// whether its key was still trusted at that position. It is not safe for
// concurrent use.
type RevocationTracker struct {
	revocations SigningKeyRevocations
	passed      map[string]bool
}

// NewRevocationTracker starts at the beginning of a log.
func NewRevocationTracker(revocations SigningKeyRevocations) *RevocationTracker {
	return &RevocationTracker{revocations: revocations, passed: map[string]bool{}}
}

// IsRevokedAt reports whether the signature of event is revoked. Call it for
// every event of the log in order, signed or not, so revocation points are
// passed.
func (tracker *RevocationTracker) IsRevokedAt(event *Event) bool {
	isRevoked := false
	if keyID, isWellFormed := SignatureKeyID(event.Signature); isWellFormed {
		if validThrough, isListed := tracker.revocations[keyID]; isListed {
			isRevoked = validThrough == "" || tracker.passed[keyID]
		}
	}
	for keyID, validThrough := range tracker.revocations {
		if validThrough != "" && validThrough == event.Hash {
			tracker.passed[keyID] = true
		}
	}
	return isRevoked
}

// MissingRevocationPoints returns revocations whose event hash the log did
// not contain, sorted by key ID. Call it after the last event.
func (tracker *RevocationTracker) MissingRevocationPoints() []SigningKeyRevocation {
	var missing []SigningKeyRevocation
	for keyID, validThrough := range tracker.revocations {
		if validThrough != "" && !tracker.passed[keyID] {
			missing = append(missing, SigningKeyRevocation{KeyID: keyID, ValidThroughHash: validThrough})
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].KeyID < missing[j].KeyID })
	return missing
}

// SignatureTrust is what signatures are verified against: the trusted public
// keys and the revocations of keys that may have been compromised.
type SignatureTrust struct {
	Keys        SigningKeySet
	Revocations SigningKeyRevocations
	// IsPinned reports that Keys were configured outside the data
	// directory. Keys read from the directory or from a backup detect
	// damage, but whoever wrote them could have added a key of their own.
	IsPinned bool
}
