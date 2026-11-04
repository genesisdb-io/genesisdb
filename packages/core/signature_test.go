package core

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
)

func newTestSigner(t testing.TB, seed byte) *EventSigner {
	t.Helper()
	if _, err := NewEventSigner(make(ed25519.PrivateKey, 10)); err == nil {
		t.Fatal("short key must be rejected")
	}
	seedBytes := make([]byte, ed25519.SeedSize)
	seedBytes[0] = seed
	signer, err := NewEventSigner(ed25519.NewKeyFromSeed(seedBytes))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestSignature_SignsTheHashTextAndVerifies(t *testing.T) {
	signer := newTestSigner(t, 1)
	event := &Event{Hash: strings.Repeat("ab", 32)}
	event.Signature = signer.SignHash(event.Hash)
	if !strings.HasPrefix(event.Signature, SignaturePrefix+signer.PublicKey().KeyID+":") {
		t.Fatalf("signature %q", event.Signature)
	}
	// The format is documented: anyone can verify with a standard library.
	_, encoded, _ := strings.Cut(strings.TrimPrefix(event.Signature, SignaturePrefix), ":")
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	public := ed25519.NewKeyFromSeed(append([]byte{1}, make([]byte, 31)...)).Public().(ed25519.PublicKey)
	if !ed25519.Verify(public, []byte(SignaturePrefix+event.Hash), raw) {
		t.Fatal("signature does not follow the documented message")
	}
	keys, err := NewSigningKeySet([]SigningPublicKey{signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	if status := keys.VerifySignature(event); status != SignatureValid {
		t.Fatalf("status %d", status)
	}
	tampered := *event
	tampered.Hash = strings.Repeat("cd", 32)
	if status := keys.VerifySignature(&tampered); status != SignatureInvalid {
		t.Fatalf("tampered hash: status %d", status)
	}
	other := newTestSigner(t, 2)
	if status := (SigningKeySet{}).VerifySignature(event); status != SignatureUnknownKey {
		t.Fatalf("empty set: status %d", status)
	}
	forged := *event
	forged.Signature = other.SignHash(event.Hash)
	forged.Signature = SignaturePrefix + signer.PublicKey().KeyID + forged.Signature[len(SignaturePrefix)+16:]
	if status := keys.VerifySignature(&forged); status != SignatureInvalid {
		t.Fatalf("signature of another key under a trusted key ID: status %d", status)
	}
	for _, malformed := range []string{"x", SignaturePrefix, SignaturePrefix + "short:00", SignaturePrefix + signer.PublicKey().KeyID + ":zz"} {
		if status := keys.VerifySignature(&Event{Hash: event.Hash, Signature: malformed}); status != SignatureInvalid {
			t.Errorf("%q: status %d", malformed, status)
		}
	}
	if status := keys.VerifySignature(&Event{Hash: event.Hash}); status != SignatureAbsent {
		t.Fatalf("unsigned: status %d", status)
	}
}

func TestSignature_ParsesPKCS8AndPublicKeys(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 7
	private := ed25519.NewKeyFromSeed(seed)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, input := range map[string][]byte{"pem": encoded, "der": der} {
		signer, err := ParseSigningKey(input)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if signer.PublicKey() != NewSigningPublicKey(private.Public().(ed25519.PublicKey)) {
			t.Fatalf("%s: wrong public key", name)
		}
	}
	if _, err := ParseSigningKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); err == nil {
		t.Fatal("public key block accepted as private key")
	}
	if _, err := ParseSigningKey([]byte("garbage")); err == nil {
		t.Fatal("garbage accepted")
	}
	publicDER, _ := x509.MarshalPKIXPublicKey(private.Public())
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	expected := NewSigningPublicKey(private.Public().(ed25519.PublicKey))
	for name, input := range map[string][]byte{"pem": publicPEM, "der": publicDER, "raw": []byte(expected.PublicKey)} {
		parsed, err := ParseSigningPublicKey(input)
		if err != nil || parsed != expected {
			t.Fatalf("%s: %+v %v", name, parsed, err)
		}
	}
	if _, err := NewSigningKeySet([]SigningPublicKey{{KeyID: "0000000000000000", PublicKey: expected.PublicKey}}); err == nil {
		t.Fatal("key ID that does not match the key accepted")
	}
}

func TestSignature_UnsignedEventsEncodeAsBefore(t *testing.T) {
	line, err := json.Marshal(&Event{Subject: "/a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "signature") {
		t.Fatalf("unsigned event encodes a signature field: %s", line)
	}
}

func BenchmarkSignHash(b *testing.B) {
	signer := newTestSigner(b, 1)
	hash := strings.Repeat("ab", 32)
	for b.Loop() {
		signer.SignHash(hash)
	}
}
