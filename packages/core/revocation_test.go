package core

import (
	"strings"
	"testing"
)

func TestRevocation_Parse(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for text, expected := range map[string]SigningKeyRevocation{
		"0123456789abcdef":                          {KeyID: "0123456789abcdef"},
		" 0123456789ABCDEF:" + hash + " ":           {KeyID: "0123456789abcdef", ValidThroughHash: hash},
		"0123456789abcdef:" + strings.ToUpper(hash): {KeyID: "0123456789abcdef", ValidThroughHash: hash},
	} {
		parsed, err := ParseSigningKeyRevocation(text)
		if err != nil || parsed != expected {
			t.Errorf("%q: %+v %v", text, parsed, err)
		}
		if again, _ := ParseSigningKeyRevocation(parsed.String()); again != parsed {
			t.Errorf("%q does not round trip", text)
		}
	}
	for _, text := range []string{"", "short", "0123456789abcdeg", "0123456789abcdef:", "0123456789abcdef:abc", "0123456789abcdef:" + strings.Repeat("zz", 32)} {
		if _, err := ParseSigningKeyRevocation(text); err == nil {
			t.Errorf("%q accepted", text)
		}
	}
	revocation := SigningKeyRevocation{KeyID: "0123456789abcdef"}
	if _, err := NewSigningKeyRevocations([]SigningKeyRevocation{revocation, revocation}); err == nil {
		t.Fatal("duplicate revocation accepted")
	}
}

func TestRevocation_TrackerFollowsTheLog(t *testing.T) {
	signer, other := newTestSigner(t, 1), newTestSigner(t, 2)
	keyID := signer.PublicKey().KeyID
	events := make([]*Event, 4)
	for position := range events {
		hash := strings.Repeat(string(rune('a'+position)), 64)
		events[position] = &Event{Hash: hash, Signature: signer.SignHash(hash)}
	}
	events[3].Signature = other.SignHash(events[3].Hash)

	partly, _ := NewSigningKeyRevocations([]SigningKeyRevocation{{KeyID: keyID, ValidThroughHash: events[1].Hash}})
	tracker := NewRevocationTracker(partly)
	var revoked []bool
	for _, event := range events {
		revoked = append(revoked, tracker.IsRevokedAt(event))
	}
	if revoked[0] || revoked[1] || !revoked[2] || revoked[3] {
		t.Fatalf("revoked after the point only, for that key only: %v", revoked)
	}
	if missing := tracker.MissingRevocationPoints(); len(missing) != 0 {
		t.Fatalf("point was passed: %v", missing)
	}

	whole, _ := NewSigningKeyRevocations([]SigningKeyRevocation{{KeyID: keyID}})
	tracker = NewRevocationTracker(whole)
	if !tracker.IsRevokedAt(events[0]) || tracker.IsRevokedAt(events[3]) {
		t.Fatal("a revocation without point revokes every signature of the key")
	}

	absent, _ := NewSigningKeyRevocations([]SigningKeyRevocation{{KeyID: keyID, ValidThroughHash: strings.Repeat("f", 64)}})
	tracker = NewRevocationTracker(absent)
	for _, event := range events {
		if tracker.IsRevokedAt(event) {
			t.Fatal("revoked before reaching the point")
		}
	}
	if missing := tracker.MissingRevocationPoints(); len(missing) != 1 || missing[0].KeyID != keyID {
		t.Fatalf("missing point not reported: %v", missing)
	}
}
